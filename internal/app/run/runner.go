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
    if _, ok := r.runningTasks.Load(task.Id); ok {
        log.WithFields(log.Fields{
            "taskID": task.Id,
        }).Error("Task is already running")
        return fmt.Errorf("task %d is already running", task.Id)
    }
    r.runningTasks.Store(task.Id, struct{}{})
    defer r.runningTasks.Delete(task.Id)

    ctx, cancel := context.WithTimeout(ctx, r.cfg.Runner.Timeout)
    defer cancel()
    reporter := report.NewReporter(ctx, cancel, r.client, task)
    var runErr error
    defer func() {
        lastWords := ""
        if runErr != nil {
            lastWords = runErr.Error()
            log.WithFields(log.Fields{
                "taskID": task.Id,
            }).Errorf("Task failed: %v", runErr)
        }
        _ = reporter.Close(lastWords)
    }()

    log.WithFields(log.Fields{
        "taskID": task.Id,
        "runner": r.name,
    }).Info("Starting task execution")

    reporter.RunDaemon()
    runErr = r.run(ctx, task, reporter)

    return nil
}

// run handles the core logic of executing a task.
func (r *Runner) run(ctx context.Context, task *runnerv1.Task, reporter *report.Reporter) (err error) {
    defer func() {
        if rec := recover(); rec != nil {
            err = fmt.Errorf("panic: %v", rec)
            log.WithFields(log.Fields{
                "taskID": task.Id,
                "runner": r.name,
            }).Errorf("Recovered from panic: %v", rec)
        }
    }()

    reporter.Logf("%s(version:%s) received task %v of job %v, triggered by event: %s",
        r.name, ver.Version(), task.Id, task.Context.Fields["job"].GetStringValue(), task.Context.Fields["event_name"].GetStringValue())

    // Generate the workflow and retrieve the job ID
    log.WithFields(log.Fields{
        "taskID": task.Id,
    }).Debug("Generating workflow for task")
    workflow, jobID, err := generateWorkflow(task)
    if err != nil {
        reporter.Logf("❌ Failed to generate workflow for task %v", task.Id)
        log.WithFields(log.Fields{
            "taskID": task.Id,
        }).Errorf("Workflow generation failed: %v", err)
        return fmt.Errorf("workflow generation failed: %v", err)
    }

    job := workflow.GetJob(jobID)
    reporter.ResetSteps(len(job.Steps))

    // Prepare Kubernetes resources needed for the workflow steps
    log.WithFields(log.Fields{
        "taskID": task.Id,
    }).Info("Preparing Kubernetes resources for the workflow")
    preset := r.preparePresetContext(task)
    giteaRuntimeToken := task.Context.Fields["gitea_runtime_token"].GetStringValue()
    if giteaRuntimeToken == "" {
        giteaRuntimeToken = preset.Token
    }
    r.envs["ACTIONS_RUNTIME_TOKEN"] = giteaRuntimeToken

    eventJSON, err := json.Marshal(preset.Event)
    if err != nil {
        reporter.Logf("❌ Failed to marshal event JSON for task %v", task.Id)
        log.WithFields(log.Fields{
            "taskID": task.Id,
        }).Errorf("Event JSON marshaling failed: %v", err)
        return fmt.Errorf("event JSON marshaling failed: %v", err)
    }

    if err := r.prepareK8sResourcesForTask(ctx, task, workflow, eventJSON, reporter); err != nil {
        reporter.Logf("❌ Failed to prepare Kubernetes resources for task %v", task.Id)
        log.WithFields(log.Fields{
            "taskID": task.Id,
        }).Errorf("Kubernetes resource preparation failed: %v", err)
        return fmt.Errorf("Kubernetes resource preparation failed: %v", err)
    }

    log.WithFields(log.Fields{
        "taskID": task.Id,
    }).Info("Executing workflow steps")

    // Execute the workflow steps
    err = r.runWorkflowSteps(ctx, task, workflow, jobID, reporter)
    if err != nil {
        reporter.Logf("❌ Workflow steps failed for task %v", task.Id)
        log.WithFields(log.Fields{
            "taskID": task.Id,
        }).Errorf("Workflow steps execution failed: %v", err)
    }

    // Cleanup Kubernetes resources after step execution
    log.WithFields(log.Fields{
        "taskID": task.Id,
    }).Info("Cleaning up Kubernetes resources for the workflow")
    if cleanupErr := r.cleanupK8sTaskResources(ctx, task, reporter); cleanupErr != nil {
        reporter.Logf("❌ Failed to cleanup Kubernetes resources for task %v", task.Id)
        log.WithFields(log.Fields{
            "taskID": task.Id,
        }).Errorf("Cleanup failed: %v", cleanupErr)
        return fmt.Errorf("cleanup failed: %v", cleanupErr)
    }

    reporter.Fire(&log.Entry{
        Time: time.Now(),
        Data: log.Fields{
            "jobResult": "success",
        },
        Message: "🏁 Workflow Completed",
    })

    log.WithFields(log.Fields{
        "taskID": task.Id,
    }).Info("Task execution completed successfully")

    return nil
}

// runWorkflowSteps runs all the steps in a given workflow job.
func (r *Runner) runWorkflowSteps(ctx context.Context, task *runnerv1.Task, workflow *model.Workflow, jobID string, reporter *report.Reporter) error {
    job := workflow.Jobs[jobID]
    steps := job.Steps

    // Default image if none is specified
    image := "catthehacker/ubuntu:act-latest"

    container := job.Container()
    if container != nil && container.Image != "" {
        image = container.Image
    }

    for stepIndex, step := range steps {
        log.WithFields(log.Fields{
            "taskID": task.Id,
            "stepIndex": stepIndex,
            "stepName": step.Name,
        }).Info("🚀 Starting Step")

        reporter.Fire(&log.Entry{
            Time: time.Now(),
            Data: log.Fields{
                "stepNumber": stepIndex,
                "stage":      "Main",
            },
            Message: fmt.Sprintf("Starting Step: %s", step.Name),
        })

        if err := r.runStepInKubernetes(ctx, task, step, stepIndex, image, reporter); err != nil {
            log.WithFields(log.Fields{
                "taskID": task.Id,
                "stepIndex": stepIndex,
                "stepName": step.Name,
            }).Errorf("❌ Step failed: %v", err)

            reporter.Fire(&log.Entry{
                Time: time.Now(),
                Data: log.Fields{
                    "stepNumber": stepIndex,
                    "stage":      "Main",
                    "stepResult": "failure",
                },
                Message: fmt.Sprintf("Step %s failed", step.Name),
            })

            if ctx.Err() == context.Canceled {
                reporter.Logf("❌ Task %d was cancelled", task.Id)
                return fmt.Errorf("task %d was cancelled", task.Id)
            }
            return err
        }

        log.WithFields(log.Fields{
            "taskID": task.Id,
            "stepIndex": stepIndex,
            "stepName": step.Name,
        }).Info("✅ Step completed successfully")

        reporter.Fire(&log.Entry{
            Time: time.Now(),
            Data: log.Fields{
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
