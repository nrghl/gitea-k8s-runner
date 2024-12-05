// k8s_exec.go

package run

import (
    "bufio"
    "context"
    "fmt"
    "io"
    "strings"
    "time"
    "gitea.com/gitea/act_runner/internal/pkg/report"
    runnerv1 "code.gitea.io/actions-proto-go/runner/v1"
    "github.com/nektos/act/pkg/model"

    log "github.com/sirupsen/logrus"
    corev1 "k8s.io/api/core/v1"
    metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
    "k8s.io/client-go/kubernetes"
)

// generatePodName constructs a unique pod name based on the task ID and step index.
func generatePodName(taskId int64, stepIndex int) string {
    return fmt.Sprintf("gitea-step-%d-%d", taskId, stepIndex)
}

// runStepInKubernetes executes a single step of the workflow within a Kubernetes pod.
func (r *Runner) runStepInKubernetes(ctx context.Context, task *runnerv1.Task, step *model.Step, stepIndex int, image string, reporter *report.Reporter) error {
    clientset, err := getKubernetesClient(ctx, reporter)
    if err != nil {
        return err
    }

    podName := generatePodName(task.Id, stepIndex)

    log.WithFields(log.Fields{
        "taskID":    task.Id,
        "stepIndex": stepIndex,
        "stepName":  step.Name,
        "podName":   podName,
    }).Info("Preparing to run step in Kubernetes")
    reporter.Logf("Preparing to run step %d (%s) for task %d in Kubernetes", stepIndex, step.Name, task.Id)

    // Define the pod spec for the step
    pod := &corev1.Pod{
        ObjectMeta: metav1.ObjectMeta{
            Name:      podName,
            Namespace: namespace,
            Labels: map[string]string{
                "app":     "k8s-act-runner",
                "task-id": fmt.Sprintf("%d", task.Id),
            },
        },
        Spec: corev1.PodSpec{
            RestartPolicy: corev1.RestartPolicyNever,
            Containers: []corev1.Container{
                {
                    Name:    "step",
                    Image:   image,
                    Command: []string{"/bin/sh", "-c"},
                    Args:    []string{step.Run},
                    EnvFrom: []corev1.EnvFromSource{
                        {
                            SecretRef: &corev1.SecretEnvSource{
                                LocalObjectReference: corev1.LocalObjectReference{
                                    Name: fmt.Sprintf("task-secrets-%d", task.Id),
                                },
                            },
                        },
                    },
                    VolumeMounts: []corev1.VolumeMount{
                        {
                            Name:      "workflow-volume",
                            MountPath: "/workspace",
                        },
                    },
                },
            },
            Volumes: []corev1.Volume{
                {
                    Name: "workflow-volume",
                    VolumeSource: corev1.VolumeSource{
                        ConfigMap: &corev1.ConfigMapVolumeSource{
                            LocalObjectReference: corev1.LocalObjectReference{
                                Name: fmt.Sprintf("workflow-files-%d", task.Id),
                            },
                        },
                    },
                },
            },
        },
    }

    // Create the Pod in Kubernetes
    _, err = clientset.CoreV1().Pods(namespace).Create(ctx, pod, metav1.CreateOptions{})
    if err != nil {
        log.WithFields(log.Fields{
            "taskID":    task.Id,
            "stepIndex": stepIndex,
            "podName":   podName,
        }).Errorf("Failed to create Pod: %v", err)
        reporter.Logf("Failed to create Pod %q for step %d (%s): %v", podName, stepIndex, step.Name, err)
        return fmt.Errorf("failed to create Pod for step %d (%s): %v", stepIndex, step.Name, err)
    }

    log.WithFields(log.Fields{
        "taskID":    task.Id,
        "stepIndex": stepIndex,
        "podName":   podName,
    }).Info("Successfully created Pod")
    reporter.Logf("Successfully created Pod %q for step %d (%s)", podName, stepIndex, step.Name)

    // Monitor the Pod's execution and stream logs to Gitea
    err = r.monitorPod(ctx, clientset, podName, namespace, stepIndex, reporter)
    if err != nil {
        log.WithFields(log.Fields{
            "taskID":    task.Id,
            "stepIndex": stepIndex,
            "podName":   podName,
        }).Errorf("Error while monitoring Pod: %v", err)
        reporter.Logf("Error while monitoring Pod %q for step %d (%s): %v", podName, stepIndex, step.Name, err)
        return err
    }

    // Cleanup the Pod after completion
    if delErr := clientset.CoreV1().Pods(namespace).Delete(ctx, podName, metav1.DeleteOptions{}); delErr != nil {
        log.WithFields(log.Fields{
            "taskID":    task.Id,
            "stepIndex": stepIndex,
            "podName":   podName,
        }).Errorf("Failed to delete Pod: %v", delErr)
        reporter.Logf("Failed to delete Pod %q: %v", podName, delErr)
    } else {
        log.WithFields(log.Fields{
            "taskID":    task.Id,
            "stepIndex": stepIndex,
            "podName":   podName,
        }).Info("Deleted Pod successfully")
        reporter.Logf("Deleted Pod %q", podName)
    }

    return nil
}

// monitorPod monitors the status of a Pod and streams its logs to the reporter.
func (r *Runner) monitorPod(ctx context.Context, clientset *kubernetes.Clientset, podName, namespace string, stepIndex int, reporter *report.Reporter) error {
    podsClient := clientset.CoreV1().Pods(namespace)
    stepLogger := log.WithField("stepNumber", stepIndex)

    log.WithFields(log.Fields{
        "podName":   podName,
        "stepIndex": stepIndex,
    }).Info("Monitoring Pod for step execution")
    reporter.Logf("Monitoring Pod %q for step %d", podName, stepIndex)

    // Continuously check the pod's status until it completes
    for {
        pod, err := podsClient.Get(ctx, podName, metav1.GetOptions{})
        if err != nil {
            stepLogger.WithField("stepResult", "failure").Errorf("Failed to get Pod: %v", err)
            reporter.Logf("Failed to get Pod %q: %v", podName, err)
            return err
        }

        phase := pod.Status.Phase
        switch phase {
        case corev1.PodSucceeded:
            stepLogger.Infof("Pod %q succeeded.", podName)
            reporter.Logf("Pod %q succeeded.", podName)
        case corev1.PodFailed:
            stepLogger.Errorf("Pod %q failed.", podName)
            reporter.Logf("Pod %q failed.", podName)
            return fmt.Errorf("Pod %q failed", podName)
        default:
            time.Sleep(500 * time.Millisecond)
        }

        if phase == corev1.PodSucceeded || phase == corev1.PodFailed {
            break
        }
    }

    log.WithFields(log.Fields{
        "podName":   podName,
        "stepIndex": stepIndex,
    }).Info("Pod has completed execution")
    reporter.Logf("Pod %q for step %d has completed", podName, stepIndex)

    // Start of log group
    reporter.Fire(&log.Entry{
        Time: time.Now(),
        Data: log.Fields{
            "stepNumber": stepIndex,
            "stage":      "Main",
            "raw_output": true,
        },
        Message: "::group::",
    })

    // Stream the logs from the pod
    podLogs, err := podsClient.GetLogs(podName, &corev1.PodLogOptions{
        Follow: true,
    }).Stream(ctx)
    if err != nil {
        stepLogger.Errorf("Error opening stream for logs: %v", err)
        reporter.Logf("Error opening stream for logs of Pod %q: %v", podName, err)
        // End the log group before returning
        reporter.Fire(&log.Entry{
            Time: time.Now(),
            Data: log.Fields{
                "stepNumber": stepIndex,
                "stage":      "Main",
                "raw_output": true,
            },
            Message: "::endgroup::",
        })
        return err
    }
    defer podLogs.Close()

    reader := bufio.NewReader(podLogs)
    for {
        line, err := reader.ReadString('\n')
        if err != nil {
            if err == io.EOF {
                break
            }
            stepLogger.Errorf("Error reading pod logs: %v", err)
            reporter.Logf("Error reading logs of Pod %q: %v", podName, err)
            // End the log group before returning
            reporter.Fire(&log.Entry{
                Time: time.Now(),
                Data: log.Fields{
                    "stepNumber": stepIndex,
                    "stage":      "Main",
                    "raw_output": true,
                },
                Message: "::endgroup::",
            })
            return err
        }

        line = strings.TrimRight(line, "\n")

        // Log locally (optional)
        log.WithFields(log.Fields{
            "stepIndex": stepIndex,
            "podName":   podName,
        }).Infof("Pod log: %s", line)

        // Report the log line to the Reporter
        reporter.Fire(&log.Entry{
            Time:    time.Now(),
            Data:    log.Fields{"stepNumber": stepIndex, "stage": "Main", "raw_output": true},
            Message: line,
        })
    }

    log.WithFields(log.Fields{
        "podName":   podName,
        "stepIndex": stepIndex,
    }).Info("Completed log streaming for Pod")
    reporter.Logf("Completed log streaming for Pod %q for step %d", podName, stepIndex)

    // End of log group
    reporter.Fire(&log.Entry{
        Time: time.Now(),
        Data: log.Fields{
            "stepNumber": stepIndex,
            "stage":      "Main",
            "raw_output": true,
        },
        Message: "::endgroup::",
    })

    return nil
}
