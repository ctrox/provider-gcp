/*
Copyright 2019 The Crossplane Authors.

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

package iam

import (
	"context"
	"fmt"

	"github.com/pkg/errors"
	iamv1 "google.golang.org/api/iam/v1"
	"google.golang.org/api/option"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/crossplane/crossplane-runtime/pkg/event"
	"github.com/crossplane/crossplane-runtime/pkg/logging"
	"github.com/crossplane/crossplane-runtime/pkg/meta"
	"github.com/crossplane/crossplane-runtime/pkg/reconciler/managed"
	"github.com/crossplane/crossplane-runtime/pkg/resource"

	"github.com/crossplane/provider-gcp/apis/iam/v1alpha1"
	gcpv1alpha3 "github.com/crossplane/provider-gcp/apis/v1alpha3"
	gcp "github.com/crossplane/provider-gcp/pkg/clients"
)

// Error strings.
const (
	errGetProvider       = "cannot get Provider"
	errProviderSecretRef = "cannot find Secret reference on Provider"
	errGetProviderSecret = "cannot get Provider Secret"
	errNewClient         = "cannot create new GCP IAM API client"
	errNotServiceAccount = "managed resource is not a GCP ServiceAccount"
	errGet               = "cannot get GCP ServiceAccount object via IAM API"
	errCreate            = "cannot create GCP ServiceAccount object via IAM API"
	errUpdate            = "cannot update GCP ServiceAccount object via IAM API"
	errDelete            = "cannot delete GCP ServiceAccount object via IAM API"
)

// SetupServiceAccount adds a controller that reconciles ServiceAccounts.
func SetupServiceAccount(mgr ctrl.Manager, l logging.Logger) error {
	name := managed.ControllerName(v1alpha1.ServiceAccountGroupKind)

	return ctrl.NewControllerManagedBy(mgr).
		Named(name).
		For(&v1alpha1.ServiceAccount{}).
		Complete(managed.NewReconciler(mgr,
			resource.ManagedKind(v1alpha1.ServiceAccountGroupVersionKind),
			managed.WithExternalConnecter(&connecter{client: mgr.GetClient(), newSAS: newServiceAccountsAPI}),
			managed.WithLogger(l.WithValues("controller", name)),
			managed.WithInitializers(managed.NewNameAsExternalName(mgr.GetClient())),
			managed.WithRecorder(event.NewAPIRecorder(mgr.GetEventRecorderFor(name)))))
}

// newServiceAccountsAPI returns a new IAM Admin Client (responsible for Service Account management).
// Credentials must be passed as JSON encoded data.
func newServiceAccountsAPI(ctx context.Context, credentials []byte) (*iamv1.ProjectsServiceAccountsService, error) {
	service, err := iamv1.NewService(ctx, option.WithCredentialsJSON(credentials))
	if err != nil {
		return nil, err
	}
	return iamv1.NewProjectsService(service).ServiceAccounts, nil
}

type connecter struct {
	client client.Client
	newSAS func(ctx context.Context, creds []byte) (*iamv1.ProjectsServiceAccountsService, error)
}

// Connect sets up iam client using credentials from the provider
func (c *connecter) Connect(ctx context.Context, mg resource.Managed) (managed.ExternalClient, error) {
	cr, ok := mg.(*v1alpha1.ServiceAccount)
	if !ok {
		return nil, errors.New(errNotServiceAccount)
	}

	p := &gcpv1alpha3.Provider{}
	if err := c.client.Get(ctx, meta.NamespacedNameOf(cr.Spec.ProviderReference), p); err != nil {
		return nil, errors.Wrap(err, errGetProvider)
	}

	if p.GetCredentialsSecretReference() == nil {
		return nil, errors.New(errProviderSecretRef)
	}

	s := &corev1.Secret{}
	n := types.NamespacedName{Namespace: p.Spec.CredentialsSecretRef.Namespace, Name: p.Spec.CredentialsSecretRef.Name}
	if err := c.client.Get(ctx, n, s); err != nil {
		return nil, errors.Wrap(err, errGetProviderSecret)
	}

	saAPI, err := c.newSAS(ctx, s.Data[p.Spec.CredentialsSecretRef.Key])
	rrn := NewRelativeResourceNamer(p.Spec.ProjectID)
	return &external{serviceAccounts: saAPI, rrn: rrn}, errors.Wrap(err, errNewClient)
}

type external struct {
	serviceAccounts *iamv1.ProjectsServiceAccountsService
	rrn             RelativeResourceNamer
}

func (e *external) Observe(ctx context.Context, mg resource.Managed) (managed.ExternalObservation, error) {
	cr, ok := mg.(*v1alpha1.ServiceAccount)
	if !ok {
		return managed.ExternalObservation{}, errors.New(errNotServiceAccount)
	}

	req := e.serviceAccounts.Get(e.rrn.ResourceName(cr))
	fromProvider, err := req.Context(ctx).Do()
	if gcp.IsErrorNotFound(err) {
		return managed.ExternalObservation{ResourceExists: false}, nil
	}
	if err != nil {
		return managed.ExternalObservation{}, errors.Wrap(err, errGet)
	}

	populateCRFromProvider(cr, fromProvider)
	return managed.ExternalObservation{
		ResourceExists:    true,
		ResourceUpToDate:  isUpToDate(&cr.Spec.ForProvider, fromProvider),
		ConnectionDetails: managed.ConnectionDetails{},
	}, nil
}

// https://cloud.google.com/iam/docs/reference/rest/v1/projects.serviceAccounts/create
// Note that the metadata.Name from the Kubernetes custom resource is used as the AccountID parameter
// All other API methods use the external-name annotation
// (set via the RelativeResourceNameAsExternalName Initializer)
func (e *external) Create(ctx context.Context, mg resource.Managed) (managed.ExternalCreation, error) {
	cr, ok := mg.(*v1alpha1.ServiceAccount)
	if !ok {
		return managed.ExternalCreation{}, errors.New(errNotServiceAccount)
	}

	csar := &iamv1.CreateServiceAccountRequest{
		AccountId: meta.GetExternalName(cr),
		ServiceAccount: &iamv1.ServiceAccount{
			DisplayName: gcp.StringValue(cr.Spec.ForProvider.DisplayName),
			Description: gcp.StringValue(cr.Spec.ForProvider.Description),
		},
	}

	// The first parameter to the Create method is the resource name of the GCP project
	// where the service account should be created
	req := e.serviceAccounts.Create(e.rrn.ProjectName(), csar)
	fromProvider, err := req.Context(ctx).Do()
	if err != nil {
		return managed.ExternalCreation{}, errors.Wrap(err, errCreate)
	}
	populateCRFromProvider(cr, fromProvider)
	return managed.ExternalCreation{}, errors.Wrap(err, errCreate)
}

// https://cloud.google.com/iam/docs/reference/rest/v1/projects.serviceAccounts/patch
func (e *external) Update(ctx context.Context, mg resource.Managed) (managed.ExternalUpdate, error) {
	cr, ok := mg.(*v1alpha1.ServiceAccount)
	if !ok {
		return managed.ExternalUpdate{}, errors.New(errNotServiceAccount)
	}

	sa := &iamv1.ServiceAccount{}
	populateProviderFromCR(sa, cr)
	psar := &iamv1.PatchServiceAccountRequest{
		ServiceAccount: sa,
		UpdateMask:     "description,displayName",
	}
	req := e.serviceAccounts.Patch(e.rrn.ResourceName(cr), psar)
	// we don't pay attention to the result of the patch request because it is only guaranteed to contain
	// `description` and `displayName` ie the fields we are trying to change
	_, err := req.Context(ctx).Do()
	return managed.ExternalUpdate{}, errors.Wrap(err, errUpdate)
}

// https://cloud.google.com/iam/docs/reference/rest/v1/projects.serviceAccounts/delete
func (e *external) Delete(ctx context.Context, mg resource.Managed) error {
	cr, ok := mg.(*v1alpha1.ServiceAccount)
	if !ok {
		return errors.New(errNotServiceAccount)
	}

	req := e.serviceAccounts.Delete(e.rrn.ResourceName(cr))
	_, err := req.Context(ctx).Do()

	if gcp.IsErrorNotFound(err) {
		return nil
	}
	return errors.Wrap(err, errDelete)
}

// isUpToDate returns true if the supplied Kubernetes resource does not differ
//  from the supplied GCP resource. It considers only fields that can be
//  modified in place without deleting and recreating the Service Account.
func isUpToDate(in *v1alpha1.ServiceAccountParameters, observed *iamv1.ServiceAccount) bool {
	// see comment in serviceaccount_types.go
	if in.DisplayName != nil && *in.DisplayName != observed.DisplayName {
		return false
	}
	if in.Description != nil && *in.Description != observed.Description {
		return false
	}
	return true
}

func populateCRFromProvider(cr *v1alpha1.ServiceAccount, fromProvider *iamv1.ServiceAccount) {
	cr.Status.AtProvider.UniqueID = fromProvider.UniqueId
	cr.Status.AtProvider.Email = fromProvider.Email
	cr.Status.AtProvider.Oauth2ClientID = fromProvider.Oauth2ClientId
	cr.Status.AtProvider.Disabled = fromProvider.Disabled
	cr.Status.AtProvider.Name = fromProvider.Name
}

func populateProviderFromCR(forProvider *iamv1.ServiceAccount, cr *v1alpha1.ServiceAccount) {
	forProvider.DisplayName = gcp.StringValue(cr.Spec.ForProvider.DisplayName)
	forProvider.Description = gcp.StringValue(cr.Spec.ForProvider.Description)
}

// NewRelativeResourceNamer makes an instance of the RelativeResourceNamer
// which is the only type that is allowed to know how to construct GCP resource names
// for the IAM type.
func NewRelativeResourceNamer(projectName string) RelativeResourceNamer {
	return RelativeResourceNamer{projectName: projectName}
}

// RelativeResourceNamer allows the controller to generate the "relative resource name"
// for the service account and GCP project based on the external-name annotation.
// https://cloud.google.com/apis/design/resource_names#relative_resource_name
// The relative resource name for service accounts has the following format:
// projects/{project_id}/serviceAccounts/{account name}
type RelativeResourceNamer struct {
	projectName string
}

// ProjectName yields the relative resource name for a GCP project
func (rrn RelativeResourceNamer) ProjectName() string {
	return fmt.Sprintf("projects/%s", rrn.projectName)
}

// ResourceName yields the relative resource name for the Service Account resource
func (rrn RelativeResourceNamer) ResourceName(sa *v1alpha1.ServiceAccount) string {
	return fmt.Sprintf("projects/%s/serviceAccounts/%s@%s.iam.gserviceaccount.com",
		rrn.projectName, meta.GetExternalName(sa), rrn.projectName)
}
