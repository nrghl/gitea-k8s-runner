// runner.go

package run

import (
    "context"
    "encoding/json"
    "fmt"
    "strings"
    "sync"
    "time"

    log "github.com/sirupsen/logrus"
    "github.com/nektos/act/pkg/model"

    runnerv1 "code.gitea.io/actions-proto-go/runner/v1"
    "connectrpc.com/connect"

    "gitea.com/gitea/act_runner/internal/pkg/client"
    "gitea.com/gitea/act_runner/internal/pkg/config"
    "gitea.com/gitea/act_runner/internal/pkg/labels"
    "gitea.com/gitea/act_runner/internal/pkg/report"
    "gitea.com/gitea/act_runner/internal/pkg/ver"
)

const (
    namespace = "gitea"
)

// Runner manages the execution of tasks/jobs.
type Runner struct {
    name             string
    cfg              *config.Config
    client           client.Client
    labels           labels.Labels
    envs             map[string]string
    runningTasks     sync.Map
    k8sTaskResources sync.Map
}

// NewRunner initializes and returns a new Runner instance.
func NewRunner(cfg *config.Config, reg *config.Registration, cli client.Client) *Runner {
    ls := labels.Labels{}
    for _, v := range reg.Labels {
        if l, err := labels.Parse(v); err == nil {
            ls = append(ls, l)
        }
    }

    envs := make(map[string]string, len(cfg.Runner.Envs))
    for k, v := range cfg.Runner.Envs {
        envs[k] = v
    }

    // Set Gitea-specific environment variables
    artifactGiteaAPI := strings.TrimSuffix(cli.Address(), "/") + "/api/actions_pipeline/"
    envs["ACTIONS_RUNTIME_URL"] = artifactGiteaAPI
    envs["ACTIONS_RESULTS_URL"] = strings.TrimSuffix(cli.Address(), "/")
    envs["GITEA_ACTIONS"] = "true"
    envs["GITEA_ACTIONS_RUNNER_VERSION"] = ver.Version()

    return &Runner{
        name:   reg.Name,
        cfg:    cfg,
        client: cli,
        labels: ls,
        envs:   envs,
    }
}

// Run executes the given task.
func (r *Runner) Run(ctx context.Context, task *runnerv1.Task) error {
    // Early error logging with logrus is acceptable when reporter is not available.
    if _, exists := r.runningTasks.Load(task.Id); exists {
        errMsg := fmt.Sprintf("Task %d is already running", task.Id)
        log.WithField("taskID", task.Id).Error(errMsg)
        return fmt.Errorf(errMsg)
    }
    r.runningTasks.Store(task.Id, struct{}{})
    defer r.runningTasks.Delete(task.Id)

    ctx, cancel := context.WithTimeout(ctx, r.cfg.Runner.Timeout)
    defer cancel()
    reporter := report.NewReporter(ctx, cancel, r.client, task)

    // Use the reporter for a user-facing start message.
    reporter.Logf("🚀 Starting task %d (runner: %s)", task.Id, r.name)

    var runErr error
    defer func() {
        lastWords := ""
        if runErr != nil {
            lastWords = runErr.Error()
            reporter.Logf("❌ Task %d failed: %v", task.Id, runErr)
        }
        _ = reporter.Close(lastWords)
    }()

    reporter.RunDaemon()
    runErr = r.run(ctx, task, reporter)
    return runErr
}

// run handles the core logic of executing a task.
func (r *Runner) run(ctx context.Context, task *runnerv1.Task, reporter *report.Reporter) (err error) {
    defer func() {
        if rec := recover(); rec != nil {
            err = fmt.Errorf("panic: %v", rec)
            // Report the panic through the reporter.
            reporter.Logf("Recovered from panic: %v", rec)
        }
    }()

    reporter.Logf("%s(version:%s) received task %v of job %v, triggered by event: %s",
        r.name, ver.Version(), task.Id,
        task.Context.Fields["job"].GetStringValue(),
        task.Context.Fields["event_name"].GetStringValue())

    // Generate the workflow and retrieve the job ID.
    reporter.Logf("Generating workflow for task %v", task.Id)
    workflow, jobID, err := generateWorkflow(task)
    if err != nil {
        reporter.Logf("❌ Failed to generate workflow for task %v: %v", task.Id, err)
        return fmt.Errorf("workflow generation failed: %v", err)
    }

    job := workflow.GetJob(jobID)
    reporter.ResetSteps(len(job.Steps))

    // Prepare Kubernetes resources needed for the workflow steps.
    reporter.Logf("Preparing Kubernetes resources for the workflow for task %v", task.Id)
    preset := r.preparePresetContext(task)
    giteaRuntimeToken := task.Context.Fields["gitea_runtime_token"].GetStringValue()
    if giteaRuntimeToken == "" {
        giteaRuntimeToken = preset.Token
    }
    r.envs["ACTIONS_RUNTIME_TOKEN"] = giteaRuntimeToken

    eventJSON, err := json.Marshal(preset.Event)
    if err != nil {
        reporter.Logf("❌ Failed to marshal event JSON for task %v: %v", task.Id, err)
        return fmt.Errorf("event JSON marshaling failed: %v", err)
    }

    if err := r.prepareK8sResourcesForTask(ctx, task, workflow, eventJSON, reporter); err != nil {
        reporter.Logf("❌ Failed to prepare Kubernetes resources for task %v: %v", task.Id, err)
        return fmt.Errorf("Kubernetes resource preparation failed: %v", err)
    }

    reporter.Logf("Executing workflow steps for task %v", task.Id)
    err = r.runWorkflowSteps(ctx, task, workflow, jobID, reporter)
    if err != nil {
        reporter.Logf("❌ Workflow steps failed for task %v: %v", task.Id, err)
    }

    // Cleanup Kubernetes resources after step execution.
    reporter.Logf("Cleaning up Kubernetes resources for the workflow for task %v", task.Id)
    if cleanupErr := r.cleanupK8sTaskResources(ctx, task, reporter); cleanupErr != nil {
        reporter.Logf("❌ Failed to cleanup Kubernetes resources for task %v: %v", task.Id, cleanupErr)
        return fmt.Errorf("cleanup failed: %v", cleanupErr)
    }

    // Signal workflow completion.
    reporter.Fire(&log.Entry{
        Time: time.Now(),
        Data: log.Fields{
            "jobResult": "success",
        },
        Message: "🏁 Workflow Completed",
    })

    reporter.Logf("Task execution completed successfully for task %v", task.Id)
    return nil
}

// runWorkflowSteps runs all the steps in a given workflow job.
func (r *Runner) runWorkflowSteps(ctx context.Context, task *runnerv1.Task, workflow *model.Workflow, jobID string, reporter *report.Reporter) error {
    job := workflow.Jobs[jobID]
    steps := job.Steps

    // Default image if none is specified.
    image := "catthehacker/ubuntu:act-latest"
    if container := job.Container(); container != nil && container.Image != "" {
        image = container.Image
    }

    for stepIndex, step := range steps {
        reporter.Fire(&log.Entry{
            Time: time.Now(),
            Data: log.Fields{
                "taskID":     task.Id,
                "stepNumber": stepIndex,
                "stage":      "Main",
            },
            Message: fmt.Sprintf("🚀 Starting Step: %s", step.Name),
        })

        if err := r.runStepInKubernetes(ctx, task, step, stepIndex, image, reporter); err != nil {
            reporter.Fire(&log.Entry{
                Time: time.Now(),
                Data: log.Fields{
                    "taskID":     task.Id,
                    "stepNumber": stepIndex,
                    "stage":      "Main",
                    "stepResult": "failure",
                },
                Message: fmt.Sprintf("❌ Step %s failed: %v", step.Name, err),
            })

            if ctx.Err() == context.Canceled {
                reporter.Logf("❌ Task %d was cancelled", task.Id)
                return fmt.Errorf("task %d was cancelled", task.Id)
            }
            return err
        }

        reporter.Fire(&log.Entry{
            Time: time.Now(),
            Data: log.Fields{
                "taskID":     task.Id,
                "stepNumber": stepIndex,
                "stage":      "Main",
                "stepResult": "success",
            },
            Message: fmt.Sprintf("✅ Success - %s", step.Name),
        })
    }
    return nil
}

// preparePresetContext constructs the GitHub context from the task's fields.
func (r *Runner) preparePresetContext(task *runnerv1.Task) *model.GithubContext {
    taskContext := task.Context.Fields
    return &model.GithubContext{
        Event:           taskContext["event"].GetStructValue().AsMap(),
        RunID:           taskContext["run_id"].GetStringValue(),
        RunNumber:       taskContext["run_number"].GetStringValue(),
        Actor:           taskContext["actor"].GetStringValue(),
        Repository:      taskContext["repository"].GetStringValue(),
        EventName:       taskContext["event_name"].GetStringValue(),
        Sha:             taskContext["sha"].GetStringValue(),
        Ref:             taskContext["ref"].GetStringValue(),
        RefName:         taskContext["ref_name"].GetStringValue(),
        RefType:         taskContext["ref_type"].GetStringValue(),
        HeadRef:         taskContext["head_ref"].GetStringValue(),
        BaseRef:         taskContext["base_ref"].GetStringValue(),
        Token:           taskContext["token"].GetStringValue(),
        RepositoryOwner: taskContext["repository_owner"].GetStringValue(),
        RetentionDays:   taskContext["retention_days"].GetStringValue(),
    }
}

// Declare registers the runner with Gitea by declaring its labels and version.
func (r *Runner) Declare(ctx context.Context, labels []string) (*connect.Response[runnerv1.DeclareResponse], error) {
    return r.client.Declare(ctx, connect.NewRequest(&runnerv1.DeclareRequest{
        Version: ver.Version(),
        Labels:  labels,
    }))
}
