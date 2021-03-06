package azure

import (
	"context"
	"fmt"
	"time"

	configv1 "github.com/openshift/api/config/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/Azure/go-autorest/autorest/azure"
	"github.com/dgrijalva/jwt-go"
	"github.com/openshift/cloud-credential-operator/pkg/operator/constants"
	secretconstants "github.com/openshift/cloud-credential-operator/pkg/operator/constants"
	"github.com/openshift/cloud-credential-operator/pkg/operator/metrics"
	"github.com/openshift/cloud-credential-operator/pkg/operator/utils"
	"github.com/openshift/cloud-credential-operator/pkg/util"

	log "github.com/sirupsen/logrus"
)

const (
	controllerName = "secretannotator"

	azureClientID       = "azure_client_id"
	azureClientSecret   = "azure_client_secret"
	azureSubscriptionID = "azure_subscription_id"
	azureTenantID       = "azure_tenant_id"
)

var _ reconcile.Reconciler = &ReconcileCloudCredSecret{}

type ReconcileCloudCredSecret struct {
	client.Client
	Logger log.FieldLogger
	Adal   AdalService
}

func NewReconciler(mgr manager.Manager) reconcile.Reconciler {
	return &ReconcileCloudCredSecret{
		Client: mgr.GetClient(),
		Logger: log.WithField("controller", secretconstants.SecretAnnotatorControllerName),
		Adal:   &adalService{},
	}
}

func Add(mgr manager.Manager, r reconcile.Reconciler) error {
	// Create a new controller
	c, err := controller.New(secretconstants.SecretAnnotatorControllerName, mgr, controller.Options{Reconciler: r})
	if err != nil {
		return err
	}

	// Watch for changes to cluster cloud secret
	p := predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			return cloudCredSecretObjectCheck(e.MetaNew)
		},
		CreateFunc: func(e event.CreateEvent) bool {
			return cloudCredSecretObjectCheck(e.Meta)
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			return cloudCredSecretObjectCheck(e.Meta)
		},
	}
	err = c.Watch(&source.Kind{Type: &corev1.Secret{}}, &handler.EnqueueRequestForObject{}, p)
	if err != nil {
		return err
	}
	return nil
}

func cloudCredSecretObjectCheck(secret metav1.Object) bool {
	return secret.GetNamespace() == secretconstants.CloudCredSecretNamespace && secret.GetName() == secretconstants.AzureCloudCredSecretName
}

func (r *ReconcileCloudCredSecret) Reconcile(request reconcile.Request) (reconcile.Result, error) {
	start := time.Now()

	r.Logger.Info("validating cloud cred secret")

	operatorIsDisabled, err := utils.IsOperatorDisabled(r.Client, r.Logger)
	if err != nil {
		r.Logger.WithError(err).Error("error checking if operator is disabled")
		return reconcile.Result{}, err
	} else if operatorIsDisabled {
		r.Logger.Infof("operator disabled in %s ConfigMap", constants.CloudCredOperatorConfigMap)
		return reconcile.Result{}, err
	}

	defer func() {
		dur := time.Since(start)
		metrics.MetricControllerReconcileTime.WithLabelValues(controllerName).Observe(dur.Seconds())
	}()

	secret := &corev1.Secret{}
	err = r.Get(context.Background(), request.NamespacedName, secret)
	if err != nil {
		r.Logger.Debugf("secret not found: %v", err)
		return reconcile.Result{}, err
	}

	err = r.validateCloudCredsSecret(secret)
	if err != nil {
		r.Logger.Errorf("error while validating cloud credentials: %v", err)
		return reconcile.Result{}, err
	}

	return reconcile.Result{}, nil
}

func (r *ReconcileCloudCredSecret) validateCloudCredsSecret(secret *corev1.Secret) error {

	if _, ok := secret.Data[azureClientID]; !ok {
		r.Logger.Errorf("Couldn't fetch key containing %v from cloud cred secret", azureClientID)
		return r.updateSecretAnnotations(secret, secretconstants.InsufficientAnnotation)
	}

	if _, ok := secret.Data[azureClientSecret]; !ok {
		r.Logger.Errorf("Couldn't fetch key containing %v from cloud cred secret", azureClientSecret)
		return r.updateSecretAnnotations(secret, secretconstants.InsufficientAnnotation)
	}

	if _, ok := secret.Data[azureTenantID]; !ok {
		r.Logger.Errorf("Couldn't fetch key containing %v from cloud cred secret", azureTenantID)
		return r.updateSecretAnnotations(secret, secretconstants.InsufficientAnnotation)
	}

	if _, ok := secret.Data[azureSubscriptionID]; !ok {
		r.Logger.Errorf("Couldn't fetch key containing %v from cloud cred secret", azureSubscriptionID)
		return r.updateSecretAnnotations(secret, secretconstants.InsufficientAnnotation)
	}

	// Can we mint new creds?
	cloudCheckResult, err := r.checkCloudCredCreation(string(secret.Data[azureTenantID]), string(secret.Data[azureClientID]), string(secret.Data[azureClientSecret]))
	if err != nil {
		r.updateSecretAnnotations(secret, secretconstants.InsufficientAnnotation)
		return fmt.Errorf("failed checking create cloud creds: %v", err)
	}

	if cloudCheckResult {
		r.Logger.Info("Verified cloud creds can be used for minting new creds")
		return r.updateSecretAnnotations(secret, secretconstants.MintAnnotation)
	}

	// else if check succeded with no error but minting is not possible, assume passthrough
	r.Logger.Info("Cloud creds will be used as-is (passthrough)")
	return r.updateSecretAnnotations(secret, secretconstants.PassthroughAnnotation)
}

func (r *ReconcileCloudCredSecret) updateSecretAnnotations(secret *corev1.Secret, value string) error {
	secretAnnotations := secret.GetAnnotations()
	if secretAnnotations == nil {
		secretAnnotations = map[string]string{}
	}

	secretAnnotations[secretconstants.AnnotationKey] = value
	secret.SetAnnotations(secretAnnotations)

	return r.Update(context.Background(), secret)
}

func (r *ReconcileCloudCredSecret) checkCloudCredCreation(tenantID, clientID, secret string) (bool, error) {
	infra := &configv1.Infrastructure{}
	if err := r.Get(context.Background(), types.NamespacedName{Name: "cluster"}, infra); err != nil {
		return false, fmt.Errorf("could not get infrastructure resource: %w", err)
	}

	cloudName := util.GetAzureCloudName(&infra.Status)
	env, err := azure.EnvironmentFromName(string(cloudName))
	if err != nil {
		return false, fmt.Errorf("unable to determine Azure environment: %w", err)
	}

	oauthConfig, err := r.Adal.NewOAuthConfig(env.ActiveDirectoryEndpoint, tenantID)
	if err != nil {
		r.Logger.WithError(err).Error("error while creating oAuthConfig")
		return false, err
	}

	token, err := r.Adal.NewServicePrincipalToken(*oauthConfig, clientID, secret, env.GraphEndpoint)
	if err != nil {
		r.Logger.WithError(err).Error("error while creating service principal")
		return false, err
	}

	p := &jwt.Parser{}
	c := &AzureClaim{}
	_, _, err = p.ParseUnverified(token.OAuthToken(), c)
	if err != nil {
		return false, err
	}

	for _, role := range c.Roles {
		if role == "Application.ReadWrite.OwnedBy" {
			return true, nil
		}
	}
	return false, nil
}
