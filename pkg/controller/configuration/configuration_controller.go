package configuration

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/gob"
	"encoding/hex"
	"fmt"
	"io/ioutil"
	"os"
	"strconv"

	cmp "github.com/google/go-cmp/cmp"

	appv1alpha1 "github.com/cvicens/rocketeer-operator/pkg/apis/app/v1alpha1"
	"github.com/go-logr/logr"
	yaml "gopkg.in/yaml.v2"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	appsv1 "k8s.io/api/apps/v1"

	oappsv1 "github.com/openshift/api/apps/v1"
	buildv1 "github.com/openshift/api/build/v1"
	imagev1 "github.com/openshift/api/image/v1"
	routev1 "github.com/openshift/api/route/v1"

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
	scheme := mgr.GetScheme()
	oappsv1.AddToScheme(scheme)
	imagev1.AddToScheme(scheme)
	routev1.AddToScheme(scheme)
	buildv1.AddToScheme(scheme)
	return &ReconcileConfiguration{client: mgr.GetClient(), scheme: scheme}
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

	var descriptorsFolder = nvl(instance.Spec.DescriptorsFolder, DEFAULT_DESCRIPTORS_FOLDER)
	var descriptorsFolderPath = fmt.Sprintf("%s/%s/", GIT_LOCAL_FOLDER, descriptorsFolder)

	configMapList := &v1.ConfigMapList{}
	if err := getAllConfigMaps(r, request, configMapList); err != nil {
		return reconcile.Result{}, err
	}

	if _, err := cloneRepository(instance.Spec.GitUrl, instance.Spec.GitRef); err != nil {
		return reconcile.Result{}, err
	}

	// Apply all descriptors
	applyDescriptorsInFolder(r, request, descriptorsFolderPath)

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

func applyDescriptorsInFolder(r *ReconcileConfiguration, request reconcile.Request, folder string) error {
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
							handleConfigMap(r, reqLogger, request.Namespace, b)
						case "Secret":
							handleSecret(r, reqLogger, request.Namespace, b)
						case "Deployment":
							handleDeployment(r, reqLogger, request.Namespace, b)
						case "DeploymentConfig":
							handleDeploymentConfig(r, reqLogger, request.Namespace, b)
						case "ImageStream":
							handleImageStream(r, reqLogger, request.Namespace, b)
						case "BuildConfig":
							handleBuildConfig(r, reqLogger, request.Namespace, b)
						case "Route":
							handleRoute(r, reqLogger, request.Namespace, b)
						case "Service":
							handleService(r, reqLogger, request.Namespace, b)
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
}

func nvl(str string, def string) string {
	if len(str) == 0 {
		return def
	}
	return str
}

func hash(buffer []byte) string {
	hasher := md5.New()
	hasher.Write(buffer)
	return hex.EncodeToString(hasher.Sum(nil))
}

func hashMapOfBytes(m map[string]byte) (string, error) {
	b := new(bytes.Buffer)
	e := gob.NewEncoder(b)

	err := e.Encode(m)
	if err != nil {
		return "", err
	}

	return hash(b.Bytes()), nil
}

func hashMapOfString(m map[string]string) (string, error) {
	b := new(bytes.Buffer)
	e := gob.NewEncoder(b)

	err := e.Encode(m)
	if err != nil {
		return "", err
	}

	return hash(b.Bytes()), nil
}

func handleConfigMap(r *ReconcileConfiguration, logger logr.Logger, namespace string, buffer []byte) error {
	logger.Info("===== ConfigMap =====")
	fromK8s := &v1.ConfigMap{}
	fromFile := &v1.ConfigMap{}
	fromFile.Namespace = namespace
	dec := k8s_yaml.NewYAMLOrJSONDecoder(bytes.NewReader(buffer), 1000)
	var err error
	if err = dec.Decode(&fromFile); err == nil {
		if err = r.client.Get(context.TODO(), types.NamespacedName{Name: fromFile.Name, Namespace: fromFile.Namespace}, fromK8s); err == nil {
			fromFile.ObjectMeta.ResourceVersion = fromK8s.ObjectMeta.ResourceVersion

			// Check if we have to update or not...
			logger.Info("+++++++++ 1 +++++++++ checkIfUpdateConfigMap")
			areEqual := checkIfUpdateConfigMap(fromFile, fromK8s)
			logger.Info("+++++++++ 2 +++++++++ checkIfUpdateConfigMap >>>>>> " + strconv.FormatBool(areEqual) + " <<<<<<<")
			if err = r.client.Update(context.TODO(), fromFile); err != nil {
				logger.Info("Update ConfigMap err: " + err.Error())
			}
		} else {
			if err = r.client.Create(context.TODO(), fromFile); err != nil {
				logger.Info("Create ConfigMap err: " + err.Error())
			}
		}
	} else {
		logger.Info("Unmarshal ConfigMap err: " + err.Error())
	}

	return err
}

func checkIfUpdateConfigMap(fromFile *v1.ConfigMap, fromK8s *v1.ConfigMap) bool {
	logger := log.WithValues("Struc", "ConfigMap")
	type intersection struct {
		Labels      map[string]string
		Annotations map[string]string
		Data        map[string]string
		BinaryData  map[string][]byte
	}

	src := &intersection{fromFile.Labels, fromFile.Annotations, fromFile.Data, fromFile.BinaryData}
	des := &intersection{fromK8s.Labels, fromK8s.Annotations, fromK8s.Data, fromK8s.BinaryData}

	if diff := cmp.Diff(src, des); diff != "" {
		logger.Info(fmt.Sprintf("mismatch (-want +got):\n%s", diff))
	}

	return cmp.Equal(src, des, nil)
}

func handleSecret(r *ReconcileConfiguration, logger logr.Logger, namespace string, buffer []byte) error {
	logger.Info("===== Secret =====")
	fromK8s := &v1.Secret{}
	fromFile := &v1.Secret{}
	fromFile.Namespace = namespace
	dec := k8s_yaml.NewYAMLOrJSONDecoder(bytes.NewReader(buffer), 1000)
	var err error
	if err = dec.Decode(&fromFile); err == nil {
		if err = r.client.Get(context.TODO(), types.NamespacedName{Name: fromFile.Name, Namespace: fromFile.Namespace}, fromK8s); err == nil {
			fromFile.ObjectMeta.ResourceVersion = fromK8s.ObjectMeta.ResourceVersion
			if err = r.client.Update(context.TODO(), fromFile); err != nil {
				logger.Info("Update Secret err: " + err.Error())
			}
		} else {
			if err = r.client.Create(context.TODO(), fromFile); err != nil {
				logger.Info("Create Secret err: " + err.Error())
			}
		}
	} else {
		logger.Info("Unmarshal Secret err: " + err.Error())
	}

	return err
}

func handleDeployment(r *ReconcileConfiguration, logger logr.Logger, namespace string, buffer []byte) error {
	logger.Info("===== Deployment =====")
	fromK8s := &appsv1.Deployment{}
	fromFile := &appsv1.Deployment{}
	fromFile.Namespace = namespace
	dec := k8s_yaml.NewYAMLOrJSONDecoder(bytes.NewReader(buffer), 1000)
	var err error
	if err = dec.Decode(&fromFile); err == nil {
		if err = r.client.Get(context.TODO(), types.NamespacedName{Name: fromFile.Name, Namespace: fromFile.Namespace}, fromK8s); err == nil {
			fromFile.ObjectMeta.ResourceVersion = fromK8s.ObjectMeta.ResourceVersion
			if err = r.client.Update(context.TODO(), fromFile); err != nil {
				logger.Info("Update Deployment err: " + err.Error())
			}
		} else {
			if err = r.client.Create(context.TODO(), fromFile); err != nil {
				logger.Info("Create Deployment err: " + err.Error())
			}
		}
	} else {
		logger.Info("Unmarshal Secret err: " + err.Error())
	}

	return err
}

func handleDeploymentConfig(r *ReconcileConfiguration, logger logr.Logger, namespace string, buffer []byte) error {
	logger.Info("===== DeploymentConfig =====")
	fromK8s := &oappsv1.DeploymentConfig{}
	fromFile := &oappsv1.DeploymentConfig{}
	fromFile.Namespace = namespace
	dec := k8s_yaml.NewYAMLOrJSONDecoder(bytes.NewReader(buffer), 1000)
	var err error
	if err = dec.Decode(&fromFile); err == nil {
		if err = r.client.Get(context.TODO(), types.NamespacedName{Name: fromFile.Name, Namespace: fromFile.Namespace}, fromK8s); err == nil {
			fromFile.ObjectMeta.ResourceVersion = fromK8s.ObjectMeta.ResourceVersion
			if err = r.client.Update(context.TODO(), fromFile); err != nil {
				logger.Info("Update DeploymentConfig err: " + err.Error())
			}
		} else {
			if err = r.client.Create(context.TODO(), fromFile); err != nil {
				logger.Info("Create DeploymentConfig err: " + err.Error())
			}
		}
	} else {
		logger.Info("Unmarshal DeploymentConfig err: " + err.Error())
	}

	return err
}

func handleImageStream(r *ReconcileConfiguration, logger logr.Logger, namespace string, buffer []byte) error {
	logger.Info("===== ImageStream =====")
	fromK8s := &imagev1.ImageStream{}
	fromFile := &imagev1.ImageStream{}
	fromFile.Namespace = namespace
	dec := k8s_yaml.NewYAMLOrJSONDecoder(bytes.NewReader(buffer), 1000)
	var err error
	if err = dec.Decode(&fromFile); err == nil {
		if err = r.client.Get(context.TODO(), types.NamespacedName{Name: fromFile.Name, Namespace: fromFile.Namespace}, fromK8s); err == nil {
			fromFile.ObjectMeta.ResourceVersion = fromK8s.ObjectMeta.ResourceVersion
			if err = r.client.Update(context.TODO(), fromFile); err != nil {
				logger.Info("Update ImageStream err: " + err.Error())
			}
		} else {
			if err = r.client.Create(context.TODO(), fromFile); err != nil {
				logger.Info("Create ImageStream err: " + err.Error())
			}
		}
	} else {
		logger.Info("Unmarshal ImageStream err: " + err.Error())
	}

	return err
}

func handleBuildConfig(r *ReconcileConfiguration, logger logr.Logger, namespace string, buffer []byte) error {
	logger.Info("===== BuildConfig =====")
	fromK8s := &buildv1.BuildConfig{}
	fromFile := &buildv1.BuildConfig{}
	fromFile.Namespace = namespace
	dec := k8s_yaml.NewYAMLOrJSONDecoder(bytes.NewReader(buffer), 1000)
	var err error
	if err = dec.Decode(&fromFile); err == nil {
		if err = r.client.Get(context.TODO(), types.NamespacedName{Name: fromFile.Name, Namespace: fromFile.Namespace}, fromK8s); err == nil {
			fromFile.ObjectMeta.ResourceVersion = fromK8s.ObjectMeta.ResourceVersion
			if err = r.client.Update(context.TODO(), fromFile); err != nil {
				logger.Info("Update BuildConfig err: " + err.Error())
			}
		} else {
			if err = r.client.Create(context.TODO(), fromFile); err != nil {
				logger.Info("Create BuildConfig err: " + err.Error())
			}
		}
	} else {
		logger.Info("Unmarshal BuildConfig err: " + err.Error())
	}

	return err
}

func handleRoute(r *ReconcileConfiguration, logger logr.Logger, namespace string, buffer []byte) error {
	logger.Info("===== Route =====")
	fromK8s := &routev1.Route{}
	fromFile := &routev1.Route{}
	fromFile.Namespace = namespace
	dec := k8s_yaml.NewYAMLOrJSONDecoder(bytes.NewReader(buffer), 1000)
	var err error
	if err = dec.Decode(&fromFile); err == nil {
		if err = r.client.Get(context.TODO(), types.NamespacedName{Name: fromFile.Name, Namespace: fromFile.Namespace}, fromK8s); err == nil {
			fromFile.ObjectMeta.ResourceVersion = fromK8s.ObjectMeta.ResourceVersion
			if err = r.client.Update(context.TODO(), fromFile); err != nil {
				logger.Info("Update Route err: " + err.Error())
			}
		} else {
			if err = r.client.Create(context.TODO(), fromFile); err != nil {
				logger.Info("Create Route err: " + err.Error())
			}
		}
	} else {
		logger.Info("Unmarshal Route err: " + err.Error())
	}

	return err
}

func handleService(r *ReconcileConfiguration, logger logr.Logger, namespace string, buffer []byte) error {
	logger.Info("===== Service =====")
	fromK8s := &corev1.Service{}
	fromFile := &corev1.Service{}
	fromFile.Namespace = namespace
	dec := k8s_yaml.NewYAMLOrJSONDecoder(bytes.NewReader(buffer), 1000)
	var err error
	if err = dec.Decode(&fromFile); err == nil {
		if err = r.client.Get(context.TODO(), types.NamespacedName{Name: fromFile.Name, Namespace: fromFile.Namespace}, fromK8s); err == nil {
			fromFile.ObjectMeta.ResourceVersion = fromK8s.ObjectMeta.ResourceVersion
			if err = r.client.Update(context.TODO(), fromFile); err != nil {
				logger.Info("Update Service err: " + err.Error())
			}
		} else {
			if err = r.client.Create(context.TODO(), fromFile); err != nil {
				logger.Info("Create Service err: " + err.Error())
			}
		}
	} else {
		logger.Info("Unmarshal Service err: " + err.Error())
	}

	return err
}
