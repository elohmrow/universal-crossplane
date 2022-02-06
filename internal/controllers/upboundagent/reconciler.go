// Copyright 2021 Upbound Inc
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package upboundagent

import (
	"context"
	"time"

	"github.com/crossplane/crossplane-runtime/pkg/logging"
	"github.com/crossplane/crossplane-runtime/pkg/meta"
	"github.com/crossplane/crossplane-runtime/pkg/resource"
	"github.com/pkg/errors"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	internalmeta "github.com/upbound/universal-crossplane/internal/meta"
)

const (
	reconcileTimeout = 1 * time.Minute

	configMapUXPVersions   = "universal-crossplane-config"
	deploymentUpboundAgent = "upbound-agent"
	keyToken               = "token"
)

const (
	errGetVersionsConfigMap = "failed to get versions config map"
	errGetSecret            = "failed to get control plane token secret"
	errDeleteDeployment     = "failed to delete agent deployment"
	errSyncDeployment       = "failed to sync agent deployment"
)

var (
	secretsKind     = schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Secret"}
	deploymentsKind = schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}
)

// ReconcilerOption is used to configure the Reconciler.
type ReconcilerOption func(*Reconciler)

// WithLogger specifies how the Reconciler should log messages.
func WithLogger(log logging.Logger) ReconcilerOption {
	return func(r *Reconciler) {
		r.log = log
	}
}

// Setup adds a controller that reconciles on control plane token secret and manages Upbound Agent deployment
func Setup(mgr ctrl.Manager, l logging.Logger, ds appsv1.DeploymentSpec, ts string) error {
	name := "upboundAgent"

	r := NewReconciler(mgr, ds, ts,
		WithLogger(l.WithValues("controller", name)),
	)

	return ctrl.NewControllerManagedBy(mgr).
		Named(name).
		For(&corev1.Secret{}).
		Owns(&appsv1.Deployment{}).
		WithEventFilter(resource.NewPredicates(resource.AnyOf(
			resource.AllOf(IsOfKind(secretsKind, mgr.GetScheme()), resource.IsNamed(ts)),
			resource.AllOf(IsOfKind(deploymentsKind, mgr.GetScheme()), resource.IsNamed(deploymentUpboundAgent)),
		))).
		Complete(r)
}

// Reconciler reconciles on control plane token secret and manages Upbound Agent deployment
type Reconciler struct {
	client         client.Client
	deploymentSpec appsv1.DeploymentSpec
	tokenSecret    string
	log            logging.Logger
}

// NewReconciler returns a new reconciler
func NewReconciler(mgr manager.Manager, ds appsv1.DeploymentSpec, ts string, opts ...ReconcilerOption) *Reconciler {
	r := &Reconciler{
		client:         mgr.GetClient(),
		deploymentSpec: ds,
		tokenSecret:    ts,
		log:            logging.NewNopLogger(),
	}

	for _, f := range opts {
		f(r)
	}

	return r
}

// Reconcile reconciles on control plane token secret and manages Upbound Agent deployment
func (r *Reconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	log := r.log.WithValues("request", req)

	log.Debug("Reconciling...")
	ctx, cancel := context.WithTimeout(ctx, reconcileTimeout)
	defer cancel()

	cm := &corev1.ConfigMap{}
	err := r.client.Get(ctx, types.NamespacedName{Name: configMapUXPVersions, Namespace: req.Namespace}, cm)

	// We create agent Deployment with an owner reference to the versions
	// ConfigMap. The agent Deployment will be garbage collected if the
	// ConfigMap no longer exists.
	if kerrors.IsNotFound(err) {
		return reconcile.Result{}, nil
	}
	if err != nil {
		return reconcile.Result{}, errors.Wrap(err, errGetVersionsConfigMap)
	}

	ts := &corev1.Secret{}
	err = r.client.Get(ctx, types.NamespacedName{Name: req.Name, Namespace: req.Namespace}, ts)

	// If the token Secret is deleted, we also want to clean up the agent
	// Deployment.
	if kerrors.IsNotFound(err) {
		err := r.client.Delete(ctx, &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      deploymentUpboundAgent,
				Namespace: cm.Namespace,
			},
		})
		// If we fail to delete agent Deployment we should immediately try
		// again. Otherwise we have nothing left to do.
		return reconcile.Result{}, errors.Wrap(err, errDeleteDeployment)
	}
	if err != nil {
		return reconcile.Result{}, errors.Wrap(err, errGetSecret)
	}

	// Ensure secret has token
	t := ts.Data[keyToken]
	if string(t) == "" {
		log.Info("Secret does not contain a token for key", "secret", r.tokenSecret, "key", keyToken)
		// We just log this as an error and do not return error since we will
		// get another update when the secret is updated with token. No need to
		// keep retrying until then.
		return reconcile.Result{}, nil
	}

	if err := r.syncAgentDeployment(ctx, cm); err != nil {
		log.Info(err.Error())
		return reconcile.Result{}, err
	}

	log.Info("Successfully synced Upbound Agent deployment!")
	return reconcile.Result{}, nil
}

func (r *Reconciler) syncAgentDeployment(ctx context.Context, cm *corev1.ConfigMap) error {
	agentDeployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      deploymentUpboundAgent,
			Namespace: cm.Namespace,
			Labels: map[string]string{
				internalmeta.LabelKeyManagedBy: internalmeta.LabelValueManagedBy,
			},
			OwnerReferences: []metav1.OwnerReference{meta.AsController(meta.TypedReferenceTo(cm, cm.GroupVersionKind()))},
		},
	}

	// crossplane runtime NewAPIUpdatingApplicator causes constant updates on the object
	// no matter it is really changed or not. This triggers another reconcile loop hence another
	// update. NewAPIPatchingApplicator does not cause above but we need update rather than
	// patch here (e.g. we removed an env var from agent deployment in an upcoming version).
	_, err := controllerutil.CreateOrUpdate(ctx, r.client, agentDeployment, func() error {
		agentDeployment.Spec = r.deploymentSpec
		return nil
	})
	return errors.Wrap(err, errSyncDeployment)
}

// IsOfKind accepts objects that are of the supplied managed resource kind.
// TODO(turkenh): move to crossplane-runtime?
func IsOfKind(k schema.GroupVersionKind, ot runtime.ObjectTyper) resource.PredicateFn {
	return func(obj runtime.Object) bool {
		gvk, err := resource.GetKind(obj, ot)
		if err != nil {
			return false
		}
		return gvk == k
	}
}
