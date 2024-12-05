// k8s_utils.go

package run

import (
    "context"
    "fmt"
    "path/filepath"
    "k8s.io/client-go/kubernetes"
    "k8s.io/client-go/rest"
    "k8s.io/client-go/tools/clientcmd"
    "k8s.io/client-go/util/homedir"

    log "github.com/sirupsen/logrus"
    "gitea.com/gitea/act_runner/internal/pkg/report"
    runnerv1 "code.gitea.io/actions-proto-go/runner/v1"
    "github.com/nektos/act/pkg/model"
    corev1 "k8s.io/api/core/v1"
    metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// k8sTaskResources holds the names of Kubernetes resources created for a task.
type k8sTaskResources struct {
    configMapName string
    secretNames   []string
}

// getKubernetesClient initializes and returns a Kubernetes clientset.
func getKubernetesClient(ctx context.Context, reporter *report.Reporter) (*kubernetes.Clientset, error) {
    config, err := rest.InClusterConfig()
    if err != nil {
        log.WithFields(log.Fields{
            "context": "kubernetes_client",
        }).Info("Not running inside a Kubernetes cluster, using ~/.kube/config")
        kubeconfig := filepath.Join(homedir.HomeDir(), ".kube", "config")
        config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
        if err != nil {
            log.WithFields(log.Fields{
                "context": "kubernetes_client",
            }).Errorf("Failed to get Kubernetes config: %v", err)
            reporter.Logf("Failed to get Kubernetes config: %v", err)
            return nil, fmt.Errorf("failed to get Kubernetes config: %v", err)
        }
    }
    clientset, err := kubernetes.NewForConfig(config)
    if err != nil {
        log.WithFields(log.Fields{
            "context": "kubernetes_client",
        }).Errorf("Failed to create Kubernetes client: %v", err)
        reporter.Logf("Failed to create Kubernetes client: %v", err)
        return nil, fmt.Errorf("failed to create Kubernetes client: %v", err)
    }
    log.WithFields(log.Fields{
        "context": "kubernetes_client",
    }).Info("Successfully created Kubernetes client")
    reporter.Logf("Successfully created Kubernetes client")
    return clientset, nil
}

// createK8sResource creates a Kubernetes ConfigMap or Secret based on the provided parameters.
func createK8sResource(ctx context.Context, clientset *kubernetes.Clientset, namespace, resourceType, name string, data map[string]string, reporter *report.Reporter) error {
    var err error
    log.WithFields(log.Fields{
        "namespace":    namespace,
        "resourceType": resourceType,
        "resourceName": name,
    }).Info("Creating Kubernetes resource")

    if resourceType == "configmap" {
        configMap := &corev1.ConfigMap{
            ObjectMeta: metav1.ObjectMeta{
                Name:      name,
                Namespace: namespace,
                Labels: map[string]string{
                    "app": "k8s-act-runner",
                },
            },
            Data: data,
        }
        _, err = clientset.CoreV1().ConfigMaps(namespace).Create(ctx, configMap, metav1.CreateOptions{})
        if err == nil {
            log.WithFields(log.Fields{
                "namespace": namespace,
                "configMap": name,
            }).Info("Created ConfigMap successfully")
            reporter.Logf("Created ConfigMap %q in namespace %q", name, namespace)
        }
    } else if resourceType == "secret" {
        secretData := make(map[string][]byte)
        for k, v := range data {
            secretData[k] = []byte(v)
        }
        secret := &corev1.Secret{
            ObjectMeta: metav1.ObjectMeta{
                Name:      name,
                Namespace: namespace,
                Labels: map[string]string{
                    "app": "k8s-act-runner",
                },
            },
            Data: secretData,
        }
        _, err = clientset.CoreV1().Secrets(namespace).Create(ctx, secret, metav1.CreateOptions{})
        if err == nil {
            log.WithFields(log.Fields{
                "namespace": namespace,
                "secret":    name,
            }).Info("Created Secret successfully")
            reporter.Logf("Created Secret %q in namespace %q", name, namespace)
        }
    }
    if err != nil {
        log.WithFields(log.Fields{
            "namespace":    namespace,
            "resourceType": resourceType,
            "resourceName": name,
        }).Errorf("Failed to create Kubernetes resource: %v", err)
        reporter.Logf("Failed to create %s %q: %v", resourceType, name, err)
        return fmt.Errorf("failed to create %s %q: %v", resourceType, name, err)
    }
    return nil
}

// prepareK8sResourcesForTask creates the necessary Kubernetes resources for the given task.
func (r *Runner) prepareK8sResourcesForTask(ctx context.Context, task *runnerv1.Task, workflow *model.Workflow, eventJSON []byte, reporter *report.Reporter) error {
    log.WithFields(log.Fields{
        "taskID": task.Id,
    }).Info("Preparing Kubernetes resources for task")
    clientset, err := getKubernetesClient(ctx, reporter)
    if err != nil {
        return err
    }

    // Prepare the ConfigMap data
    workflowConfigMapName := fmt.Sprintf("workflow-files-%d", task.Id)
    workflowData := map[string]string{"workflow.json": string(eventJSON)}

    // Create ConfigMap for the workflow
    if err := createK8sResource(ctx, clientset, namespace, "configmap", workflowConfigMapName, workflowData, reporter); err != nil {
        return err
    }

    // Create Secret for the task
    secretName := fmt.Sprintf("task-secrets-%d", task.Id)
    if err := createK8sResource(ctx, clientset, namespace, "secret", secretName, task.Secrets, reporter); err != nil {
        return err
    }

    log.WithFields(log.Fields{
        "taskID":           task.Id,
        "configMapName":    workflowConfigMapName,
        "secretName":       secretName,
    }).Info("Kubernetes resources for task prepared successfully")

    // Track resources for cleanup
    r.trackK8sResourcesForCleanup(task.Id, workflowConfigMapName, secretName)

    return nil
}

// trackK8sResourcesForCleanup stores the names of Kubernetes resources created for a task.
func (r *Runner) trackK8sResourcesForCleanup(taskId int64, configMapName string, secretNames ...string) {
    log.WithFields(log.Fields{
        "taskID":        taskId,
        "configMapName": configMapName,
        "secretNames":   secretNames,
    }).Debug("Tracking Kubernetes resources for cleanup")
    r.k8sTaskResources.Store(taskId, k8sTaskResources{
        configMapName: configMapName,
        secretNames:   secretNames,
    })
}

// cleanupK8sTaskResources removes the Kubernetes resources associated with a task.
func (r *Runner) cleanupK8sTaskResources(ctx context.Context, task *runnerv1.Task, reporter *report.Reporter) error {
    log.WithFields(log.Fields{
        "taskID": task.Id,
    }).Info("Cleaning up Kubernetes resources for task")
    clientset, err := getKubernetesClient(ctx, reporter)
    if err != nil {
        log.WithFields(log.Fields{
            "taskID": task.Id,
        }).Errorf("Failed to create Kubernetes client during cleanup: %v", err)
        return fmt.Errorf("failed to create Kubernetes client: %v", err)
    }

    if resources, ok := r.k8sTaskResources.Load(task.Id); ok {
        res := resources.(k8sTaskResources)

        // Delete ConfigMap
        if res.configMapName != "" {
            if err := clientset.CoreV1().ConfigMaps(namespace).Delete(ctx, res.configMapName, metav1.DeleteOptions{}); err != nil {
                log.WithFields(log.Fields{
                    "taskID":       task.Id,
                    "configMapName": res.configMapName,
                }).Errorf("Failed to delete ConfigMap: %v", err)
                reporter.Logf("Failed to delete ConfigMap %q: %v", res.configMapName, err)
            } else {
                log.WithFields(log.Fields{
                    "taskID":       task.Id,
                    "configMapName": res.configMapName,
                }).Info("Deleted ConfigMap successfully")
                reporter.Logf("Deleted ConfigMap %q", res.configMapName)
            }
        }

        // Delete Secrets
        for _, secretName := range res.secretNames {
            if err := clientset.CoreV1().Secrets(namespace).Delete(ctx, secretName, metav1.DeleteOptions{}); err != nil {
                log.WithFields(log.Fields{
                    "taskID":    task.Id,
                    "secretName": secretName,
                }).Errorf("Failed to delete Secret: %v", err)
                reporter.Logf("Failed to delete Secret %q: %v", secretName, err)
            } else {
                log.WithFields(log.Fields{
                    "taskID":    task.Id,
                    "secretName": secretName,
                }).Info("Deleted Secret successfully")
                reporter.Logf("Deleted Secret %q", secretName)
            }
        }

        // Remove the resources from the tracking map
        r.k8sTaskResources.Delete(task.Id)
        log.WithFields(log.Fields{
            "taskID": task.Id,
        }).Debug("Removed Kubernetes resources from tracking map")
    }

    return nil
}
