// SPDX-FileCopyrightText: 2023 The Crossplane Authors <https://crossplane.io>
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"

	"github.com/crossplane/upjet/pkg/controller/handler"
	"github.com/crossplane/upjet/pkg/resource"
	"github.com/crossplane/upjet/pkg/terraform"
	"github.com/pkg/errors"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrl "sigs.k8s.io/controller-runtime/pkg/manager"

	xpv1 "github.com/crossplane/crossplane-runtime/apis/common/v1"
	xpresource "github.com/crossplane/crossplane-runtime/pkg/resource"
)

const (
	errGetFmt              = "cannot get resource %s/%s after an async %s"
	errUpdateStatusFmt     = "cannot update status of the resource %s/%s after an async %s"
	errReconcileRequestFmt = "cannot request the reconciliation of the resource %s/%s after an async %s"
)

const (
	rateLimiterCallback = "asyncCallback"
)

var _ CallbackProvider = &APICallbacks{}

// APISecretClient is a client for getting k8s secrets
type APISecretClient struct {
	kube client.Client
}

// GetSecretData gets and returns data for the referenced secret
func (a *APISecretClient) GetSecretData(ctx context.Context, ref *xpv1.SecretReference) (map[string][]byte, error) {
	secret := &v1.Secret{}
	if err := a.kube.Get(ctx, types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}, secret); err != nil {
		return nil, err
	}
	return secret.Data, nil
}

// GetSecretValue gets and returns value for key of the referenced secret
func (a *APISecretClient) GetSecretValue(ctx context.Context, sel xpv1.SecretKeySelector) ([]byte, error) {
	d, err := a.GetSecretData(ctx, &sel.SecretReference)
	if err != nil {
		return nil, errors.Wrap(err, "cannot get secret data")
	}
	return d[sel.Key], err
}

// APICallbacksOption represents a configurable option for the APICallbacks
type APICallbacksOption func(callbacks *APICallbacks)

// WithEventHandler sets the EventHandler for the APICallbacks so that
// the APICallbacks instance can requeue reconcile requests in the
// context of the asynchronous operations.
func WithEventHandler(e *handler.EventHandler) APICallbacksOption {
	return func(callbacks *APICallbacks) {
		callbacks.eventHandler = e
	}
}

// NewAPICallbacks returns a new APICallbacks.
func NewAPICallbacks(m ctrl.Manager, of xpresource.ManagedKind, opts ...APICallbacksOption) *APICallbacks {
	nt := func() resource.Terraformed {
		return xpresource.MustCreateObject(schema.GroupVersionKind(of), m.GetScheme()).(resource.Terraformed)
	}
	cb := &APICallbacks{
		kube:           m.GetClient(),
		newTerraformed: nt,
	}
	for _, o := range opts {
		o(cb)
	}
	return cb
}

// APICallbacks providers callbacks that work on API resources.
type APICallbacks struct {
	eventHandler *handler.EventHandler

	kube           client.Client
	newTerraformed func() resource.Terraformed
}

func (ac *APICallbacks) callbackFn(name, op string, requeue bool) terraform.CallbackFn {
	return func(err error, ctx context.Context) error {
		nn := types.NamespacedName{Name: name}
		tr := ac.newTerraformed()
		if kErr := ac.kube.Get(ctx, nn, tr); kErr != nil {
			return errors.Wrapf(kErr, errGetFmt, tr.GetObjectKind().GroupVersionKind().String(), name, op)
		}
		tr.SetConditions(resource.LastAsyncOperationCondition(err))
		tr.SetConditions(resource.AsyncOperationFinishedCondition())
		uErr := errors.Wrapf(ac.kube.Status().Update(ctx, tr), errUpdateStatusFmt, tr.GetObjectKind().GroupVersionKind().String(), name, op)
		if ac.eventHandler != nil && requeue {
			switch {
			case err != nil:
				// TODO: use the errors.Join from
				// github.com/crossplane/crossplane-runtime.
				if ok := ac.eventHandler.RequestReconcile(rateLimiterCallback, name, nil); !ok {
					return errors.Errorf(errReconcileRequestFmt, tr.GetObjectKind().GroupVersionKind().String(), name, op)
				}
			default:
				ac.eventHandler.Forget(rateLimiterCallback, name)
			}
		}
		return uErr
	}
}

// Create makes sure the error is saved in async operation condition.
func (ac *APICallbacks) Create(name string) terraform.CallbackFn {
	return func(err error, ctx context.Context) error {
		// requeue is set to true although the managed reconciler already
		// requeues with exponential back-off during the creation phase
		// because the upjet external client returns ResourceExists &
		// ResourceUpToDate both set to true, if an async operation is
		// in-progress immediately following a Create call. This will
		// delay a reobservation of the resource (while being created)
		// for the poll period.
		return ac.callbackFn(name, "create", true)(err, ctx)
	}
}

// Update makes sure the error is saved in async operation condition.
func (ac *APICallbacks) Update(name string) terraform.CallbackFn {
	return func(err error, ctx context.Context) error {
		return ac.callbackFn(name, "update", true)(err, ctx)
	}
}

// Destroy makes sure the error is saved in async operation condition.
func (ac *APICallbacks) Destroy(name string) terraform.CallbackFn {
	// requeue is set to false because the managed reconciler already requeues
	// with exponential back-off during the deletion phase.
	return ac.callbackFn(name, "destroy", false)
}
