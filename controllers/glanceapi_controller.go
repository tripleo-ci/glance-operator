/*


Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"context"

	"github.com/go-logr/logr"
	glancev1beta1 "github.com/openstack-k8s-operators/glance-operator/api/v1beta1"
	glance "github.com/openstack-k8s-operators/glance-operator/pkg"
	util "github.com/openstack-k8s-operators/lib-common/pkg/util"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	k8s_errors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"fmt"
	"reflect"
	"time"
)

// GlanceAPIReconciler reconciles a GlanceAPI object
type GlanceAPIReconciler struct {
	client.Client
	Log    logr.Logger
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=glance.openstack.org,resources=glanceapis,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=glance.openstack.org,resources=glanceapis/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;create;update;delete;
// +kubebuilder:rbac:groups=core,resources=services,verbs=get;list;create;update;delete;
// +kubebuilder:rbac:groups=core,resources=persistentvolumeclaims,verbs=get;list;create;update;delete;
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;create;update;delete;
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;create;update;delete;
func (r *GlanceAPIReconciler) Reconcile(req ctrl.Request) (ctrl.Result, error) {
	_ = context.Background()
	_ = r.Log.WithValues("glanceapi", req.NamespacedName)

	// Fetch the Glance instance
	instance := &glancev1beta1.GlanceAPI{}
	err := r.Client.Get(context.TODO(), req.NamespacedName, instance)
	if err != nil {
		if k8s_errors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected.
			// For additional cleanup logic use finalizers. Return and don't requeue.
			return ctrl.Result{}, nil
		}
		// Error reading the object - requeue the request.
		return ctrl.Result{}, err
	}

	// PVC
	pvc := glance.Pvc(instance, r.Scheme)

	foundPvc := &corev1.PersistentVolumeClaim{}
	err = r.Client.Get(context.TODO(), types.NamespacedName{Name: pvc.Name, Namespace: pvc.Namespace}, foundPvc)
	if err != nil && k8s_errors.IsNotFound(err) {

		r.Log.Info("Creating a new Pvc", "PersistentVolumeClaim.Namespace", pvc.Namespace, "Service.Name", pvc.Name)
		err = r.Client.Create(context.TODO(), pvc)
		if err != nil {
			return ctrl.Result{}, err
		}

		return ctrl.Result{RequeueAfter: time.Second * 5}, err
	} else if err != nil {
		return ctrl.Result{}, err
	}

	service := glance.Service(instance, r.Scheme)

	// Check if this Service already exists
	foundService := &corev1.Service{}
	err = r.Client.Get(context.TODO(), types.NamespacedName{Name: service.Name, Namespace: service.Namespace}, foundService)
	if err != nil && k8s_errors.IsNotFound(err) {

		r.Log.Info("Creating a new Service", "Service.Namespace", service.Namespace, "Service.Name", service.Name)
		err = r.Client.Create(context.TODO(), service)
		if err != nil {
			return ctrl.Result{}, err
		}

		return ctrl.Result{RequeueAfter: time.Second * 5}, err
	} else if err != nil {
		return ctrl.Result{}, err
	}

	// ConfigMap
	configMap := glance.ConfigMap(instance, r.Scheme)
	// Check if this ConfigMap already exists
	foundConfigMap := &corev1.ConfigMap{}
	err = r.Client.Get(context.TODO(), types.NamespacedName{Name: configMap.Name, Namespace: configMap.Namespace}, foundConfigMap)
	if err != nil && k8s_errors.IsNotFound(err) {
		r.Log.Info("Creating a new ConfigMap", "ConfigMap.Namespace", configMap.Namespace, "Job.Name", configMap.Name)
		err = r.Client.Create(context.TODO(), configMap)
		if err != nil {
			return ctrl.Result{}, err
		}
	} else if !reflect.DeepEqual(configMap.Data, foundConfigMap.Data) {
		r.Log.Info("Updating ConfigMap")
		foundConfigMap.Data = configMap.Data
		err = r.Client.Update(context.TODO(), foundConfigMap)
		if err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: time.Second * 5}, err
	}

	// Create the DB Schema (unstructured so we don't explicitly import mariadb-operator code)
	schemaObj, err := glance.SchemaObject(instance)
	if err != nil {
		return ctrl.Result{}, err
	}

	foundSchema := &unstructured.Unstructured{}
	foundSchema.SetGroupVersionKind(schemaObj.GroupVersionKind())
	err = r.Client.Get(context.TODO(), types.NamespacedName{Name: schemaObj.GetName(), Namespace: schemaObj.GetNamespace()}, foundSchema)
	if err != nil && k8s_errors.IsNotFound(err) {
		err := r.Client.Create(context.TODO(), &schemaObj)
		if err != nil {
			return ctrl.Result{}, err
		}
	} else if err != nil {
		return ctrl.Result{}, err
	} else {
		completed, _, err := unstructured.NestedBool(foundSchema.UnstructuredContent(), "status", "completed")
		if !completed {
			r.Log.Info("Waiting on DB to be created...")
			return ctrl.Result{RequeueAfter: time.Second * 5}, err
		}
	}

	// Define a new Job object
	job := glance.DbSyncJob(instance, r.Scheme)
	dbSyncHash, err := util.ObjectHash(job)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("error calculating DB sync hash: %v", err)
	}

	requeue := true
	if instance.Status.DbSyncHash != dbSyncHash {
		requeue, err = glance.EnsureJob(job, r.Client, r.Log)
		r.Log.Info("Running DB sync")
		if err != nil {
			return ctrl.Result{}, err
		} else if requeue {
			r.Log.Info("Waiting on DB sync")
			return ctrl.Result{RequeueAfter: time.Second * 5}, err
		}
	}
	// db sync completed... okay to store the hash to disable it
	if err := r.setDbSyncHash(instance, dbSyncHash); err != nil {
		return ctrl.Result{}, err
	}
	// delete the job
	requeue, err = glance.DeleteJob(job, r.Client, r.Log)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Define a new Deployment object
	configMapHash, err := util.ObjectHash(configMap)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("error calculating config map hash: %v", err)
	}
	r.Log.Info("ConfigMapHash: ", "Data Hash:", configMapHash)
	deployment := glance.Deployment(instance, configMapHash, r.Scheme)
	deploymentHash, err := util.ObjectHash(deployment)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("error deployment hash: %v", err)
	}
	r.Log.Info("DeploymentHash: ", "Deployment Hash:", deploymentHash)

	// Check if this Deployment already exists
	foundDeployment := &appsv1.Deployment{}
	err = r.Client.Get(context.TODO(), types.NamespacedName{Name: deployment.Name, Namespace: deployment.Namespace}, foundDeployment)
	if err != nil && k8s_errors.IsNotFound(err) {
		r.Log.Info("Creating a new Deployment", "Deployment.Namespace", deployment.Namespace, "Deployment.Name", deployment.Name)
		err = r.Client.Create(context.TODO(), deployment)
		if err != nil {
			return ctrl.Result{}, err
		}

		return ctrl.Result{RequeueAfter: time.Second * 10}, err

	} else if err != nil {
		return ctrl.Result{}, err
	} else {

		if instance.Status.DeploymentHash != deploymentHash {
			r.Log.Info("Deployment Updated")
			foundDeployment.Spec = deployment.Spec
			err = r.Client.Update(context.TODO(), foundDeployment)
			if err != nil {
				return ctrl.Result{}, err
			}
			if err := r.setDeploymentHash(instance, deploymentHash); err != nil {
				return ctrl.Result{}, err
			}

			return ctrl.Result{RequeueAfter: time.Second * 10}, err
		}
		if foundDeployment.Status.ReadyReplicas == instance.Spec.Replicas {
			r.Log.Info("Deployment Replicas running:", "Replicas", foundDeployment.Status.ReadyReplicas)
		} else {
			r.Log.Info("Waiting on Glance Deployment...")
			return ctrl.Result{RequeueAfter: time.Second * 5}, err
		}
	}

	return ctrl.Result{}, nil
}

func (r *GlanceAPIReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&glancev1beta1.GlanceAPI{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Owns(&appsv1.Deployment{}).
		Complete(r)
}

func (r *GlanceAPIReconciler) setDbSyncHash(api *glancev1beta1.GlanceAPI, hashStr string) error {

	if hashStr != api.Status.DbSyncHash {
		api.Status.DbSyncHash = hashStr
		if err := r.Client.Status().Update(context.TODO(), api); err != nil {
			return err
		}
	}
	return nil
}

func (r *GlanceAPIReconciler) setDeploymentHash(instance *glancev1beta1.GlanceAPI, hashStr string) error {

	if hashStr != instance.Status.DeploymentHash {
		instance.Status.DeploymentHash = hashStr
		if err := r.Client.Status().Update(context.TODO(), instance); err != nil {
			return err
		}
	}
	return nil

}
