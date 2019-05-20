package configuration

import (
	"bytes"
	"context"
	"fmt"
	"io/ioutil"
	"os"

	appv1alpha1 "github.com/cvicens/rocketeer-operator/pkg/apis/app/v1alpha1"
	yaml "gopkg.in/yaml.v2"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	k8s_yaml "k8s.io/apimachinery/pkg/util/yaml"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	logf "sigs.k8s.io/controller-runtime/pkg/runtime/log"
	"sigs.k8s.io/controller-runtime/pkg/source"

	git "gopkg.in/src-d/go-git.v4"
	plumbing "gopkg.in/src-d/go-git.v4/plumbing"
)

var log = logf.Log.WithName("controller_configuration")

const GIT_LOCAL_FOLDER = "./tmp"
const DEFAULT_DESCRIPTORS_FOLDER = "k8s"

/**
* USER ACTION REQUIRED: This is a scaffold file intended for the user to modify with their own Controller
* business logic.  Delete these comments after modifying this file.*
 */

// Add creates a new Configuration Controller and adds it to the Manager. The Manager will set fields on the Controller
// and Start it when the Manager is Started.
func Add(mgr manager.Manager) error {
	return add(mgr, newReconciler(mgr))
}

// newReconciler returns a new reconcile.Reconciler
func newReconciler(mgr manager.Manager) reconcile.Reconciler {
	return &ReconcileConfiguration{client: mgr.GetClient(), scheme: mgr.GetScheme()}
}

// add adds a new Controller to mgr with r as the reconcile.Reconciler
func add(mgr manager.Manager, r reconcile.Reconciler) error {
	// Create a new controller
	c, err := controller.New("configuration-controller", mgr, controller.Options{Reconciler: r})
	if err != nil {
		return err
	}

	// Watch for changes to primary resource Configuration
	err = c.Watch(&source.Kind{Type: &appv1alpha1.Configuration{}}, &handler.EnqueueRequestForObject{})
	if err != nil {
		return err
	}

	// TODO(user): Modify this to be the types you create that are owned by the primary resource
	// Watch for changes to secondary resource Pods and requeue the owner Configuration
	err = c.Watch(&source.Kind{Type: &corev1.Pod{}}, &handler.EnqueueRequestForOwner{
		IsController: true,
		OwnerType:    &appv1alpha1.Configuration{},
	})
	if err != nil {
		return err
	}

	return nil
}

// blank assignment to verify that ReconcileConfiguration implements reconcile.Reconciler
var _ reconcile.Reconciler = &ReconcileConfiguration{}

// ReconcileConfiguration reconciles a Configuration object
type ReconcileConfiguration struct {
	// This client, initialized using mgr.Client() above, is a split client
	// that reads objects from the cache and writes to the apiserver
	client client.Client
	scheme *runtime.Scheme
}

// Reconcile reads that state of the cluster for a Configuration object and makes changes based on the state read
// and what is in the Configuration.Spec
// TODO(user): Modify this Reconcile function to implement your Controller logic.  This example creates
// a Pod as an example
// Note:
// The Controller will requeue the Request to be processed again if the returned error is non-nil or
// Result.Requeue is true, otherwise upon completion it will remove the work from the queue.
func (r *ReconcileConfiguration) Reconcile(request reconcile.Request) (reconcile.Result, error) {
	reqLogger := log.WithValues("Request.Namespace", request.Namespace, "Request.Name", request.Name)
	reqLogger.Info("Reconciling Configuration")

	// Fetch the Configuration instance
	instance := &appv1alpha1.Configuration{}
	err := r.client.Get(context.TODO(), request.NamespacedName, instance)
	if err != nil {
		if errors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			return reconcile.Result{}, nil
		}
		// Error reading the object - requeue the request.
		return reconcile.Result{}, err
	}

	var descriptorsFolder = NVL(instance.Spec.DescriptorsFolder, DEFAULT_DESCRIPTORS_FOLDER)
	var descriptorsFolderPath = fmt.Sprintf("%s/%s/", GIT_LOCAL_FOLDER, descriptorsFolder)

	configMapList := &v1.ConfigMapList{}
	if err := getAllConfigMaps(r, request, configMapList); err != nil {
		return reconcile.Result{}, err
	}
	//reqLogger.Info("getAllConfigMaps", "configmapList.Items", configMapList.Items)

	if _, err := cloneRepository(instance.Spec.GitUrl, instance.Spec.GitRef); err != nil {
		return reconcile.Result{}, err
	}

	// Apply all configmaps
	applyAllFilesInFolder(r, request, descriptorsFolderPath)

	// Define a new Pod object
	pod := newPodForCR(instance)

	// Set Configuration instance as the owner and controller
	if err := controllerutil.SetControllerReference(instance, pod, r.scheme); err != nil {
		return reconcile.Result{}, err
	}

	// Check if this Pod already exists
	found := &corev1.Pod{}
	err = r.client.Get(context.TODO(), types.NamespacedName{Name: pod.Name, Namespace: pod.Namespace}, found)
	if err != nil && errors.IsNotFound(err) {
		reqLogger.Info("Creating a new Pod", "Pod.Namespace", pod.Namespace, "Pod.Name", pod.Name)
		err = r.client.Create(context.TODO(), pod)
		if err != nil {
			return reconcile.Result{}, err
		}

		// Pod created successfully - don't requeue
		return reconcile.Result{}, nil
	} else if err != nil {
		return reconcile.Result{}, err
	}

	// Pod already exists - don't requeue
	reqLogger.Info("Skip reconcile: Pod already exists", "Pod.Namespace", found.Namespace, "Pod.Name", found.Name)
	return reconcile.Result{}, nil
}

// newPodForCR returns a busybox pod with the same name/namespace as the cr
func newPodForCR(cr *appv1alpha1.Configuration) *corev1.Pod {
	labels := map[string]string{
		"app": cr.Name,
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cr.Name + "-pod",
			Namespace: cr.Namespace,
			Labels:    labels,
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:    "busybox",
					Image:   "busybox",
					Command: []string{"sleep", "3600"},
				},
			},
		},
	}
}

func getAllConfigMaps(r *ReconcileConfiguration, request reconcile.Request, configmapList *corev1.ConfigMapList) error {
	// Return all configmaps in the request namespace with a label of `app=<name>`
	opts := &client.ListOptions{}
	//opts.SetLabelSelector(fmt.Sprintf("app=%s", request.NamespacedName.Name))
	opts.InNamespace(request.NamespacedName.Namespace)

	ctx := context.TODO()
	err := r.client.List(ctx, opts, configmapList)

	return err
}

func cloneRepository(url string, ref string) (*git.Repository, error) {
	if repo, err := git.PlainOpen(GIT_LOCAL_FOLDER); err == nil {
		if w, err := repo.Worktree(); err == nil {
			if err := w.Pull(&git.PullOptions{RemoteName: "origin"}); err == nil || err.Error() == "already up-to-date" {
				return repo, nil
			} else {
				os.RemoveAll(GIT_LOCAL_FOLDER)
				return nil, err
			}
		} else {
			os.RemoveAll(GIT_LOCAL_FOLDER)
			return nil, err
		}
	} else {
		// Delete just in case
		os.RemoveAll(GIT_LOCAL_FOLDER)
		// Clone
		repo, err := git.PlainClone(GIT_LOCAL_FOLDER, false, &git.CloneOptions{
			URL:               url,
			ReferenceName:     plumbing.ReferenceName("refs/heads/" + ref),
			RecurseSubmodules: git.DefaultSubmoduleRecursionDepth,
		})
		return repo, err
	}
}

func applyAllFilesInFolder(r *ReconcileConfiguration, request reconcile.Request, folder string) error {
	reqLogger := log.WithValues("Request.Namespace", request.Namespace, "Request.Name", request.Name)

	if files, err := ioutil.ReadDir(folder); err == nil {
		for _, f := range files {
			reqLogger.Info("current file: " + f.Name())
			if b, err := ioutil.ReadFile(folder + f.Name()); err == nil {
				typeMeta := &metav1.TypeMeta{}
				if err := yaml.Unmarshal([]byte(b), &typeMeta); err == nil {
					reqLogger.Info("typeMeta: " + typeMeta.String())

					objectMeta := &metav1.ObjectMeta{}
					if err := yaml.Unmarshal([]byte(b), &objectMeta); err == nil {
						objectMeta.Namespace = request.Namespace
						reqLogger.Info("objectMeta: " + objectMeta.String())
						switch kind := typeMeta.GetObjectKind().GroupVersionKind().Kind; kind {
						case "ConfigMap":
							reqLogger.Info("===== isConfigMap =====")
							configMap := &v1.ConfigMap{}
							configMap.Namespace = request.Namespace
							dec := k8s_yaml.NewYAMLOrJSONDecoder(bytes.NewReader([]byte(b)), 1000)
							if err := dec.Decode(&configMap); err == nil {
								reqLogger.Info("configMap", "name", objectMeta.Name, "data", configMap.String())

								aux := &v1.ConfigMap{}
								if err := r.client.Get(context.TODO(), types.NamespacedName{Name: configMap.Name, Namespace: configMap.Namespace}, aux); err == nil {
									if err := r.client.Update(context.TODO(), configMap); err != nil {
										reqLogger.Info("Update ConfigMap err: " + err.Error())
									}
								} else {
									if err := r.client.Create(context.TODO(), configMap); err != nil {
										reqLogger.Info("Create ConfigMap err: " + err.Error())
									}
								}
							} else {
								reqLogger.Info("Unmarshal configMap err: " + err.Error())
							}
						case "Secret":
							reqLogger.Info("===== isSecret =====")
							secret := &v1.Secret{}
							if err := yaml.Unmarshal([]byte(b), &secret); err == nil {
								reqLogger.Info("secret", "name", secret.Name, "data", secret.Data)
							} else {
								reqLogger.Info("Unmarshal secret err: " + err.Error())
							}
						default:
							reqLogger.Info("===== isOther =====" + kind)
						}
					} else {
						reqLogger.Info("Unmarshall ObjectMeta error: " + err.Error())
					}
				} else {
					reqLogger.Info("Unmarshall TypeMeta error: " + err.Error())
				}

			} else {
				reqLogger.Info("ReadFile error: " + err.Error())
			}
		}
	} else {
		reqLogger.Info("ReadDir error: " + err.Error())
	}

	return nil

	/*u := &unstructured.Unstructured{}
	u.Object = map[string]interface{}{
		"name":      "name",
		"namespace": "namespace",
		"spec": map[string]interface{}{
			"replicas": 2,
			"selector": map[string]interface{}{
				"matchLabels": map[string]interface{}{
					"foo": "bar",
				},
			},
			"template": map[string]interface{}{
				"labels": map[string]interface{}{
					"foo": "bar",
				},
				"spec": map[string]interface{}{
					"containers": []map[string]interface{}{
						{
							"name":  "nginx",
							"image": "nginx",
						},
					},
				},
			},
		},
	}
	u.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "apps",
		Kind:    "Deployment",
		Version: "v1",
	})
	_ = c.Create(context.Background(), u)*/

	/*ctx := context.TODO()
	err := r.client.List(ctx, opts, configmapList)

	if err := r.client.Create(context.TODO(), pod); err != nil {
		return reconcile.Result{}, err
	}

	return err*/
}

func NVL(str string, def string) string {
	if len(str) == 0 {
		return def
	}
	return str
}
