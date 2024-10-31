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
	"fmt"
	"github.com/go-logr/logr"
	"github.com/opendatahub-io/odh-model-controller/api/nim/v1"
	"github.com/opendatahub-io/odh-model-controller/controllers/constants"
	"github.com/opendatahub-io/odh-model-controller/controllers/utils"
	templatev1 "github.com/openshift/api/template/v1"
	patchutils "github.com/rhecosystemappeng/patch-utils/pkg"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

type (
	NimAccountReconciler struct {
		client.Client
		Log logr.Logger
	}

	conditionType string
)

const (
	apiKeySpecPath = "spec.apiKeySecret.name"

	condAccountStatus    conditionType = "AccountStatus"
	condTemplateUpdate   conditionType = "TemplateUpdate"
	condSecretUpdate     conditionType = "SecretUpdate"
	condConfigMapUpdate  conditionType = "ConfigMapUpdate"
	condAPIKeyValidation conditionType = "APIKeyValidation"
)

func (r *NimAccountReconciler) SetupWithManager(mgr ctrl.Manager, ctx context.Context) error {
	if err := mgr.GetFieldIndexer().IndexField(ctx, &v1.Account{}, apiKeySpecPath, func(obj client.Object) []string {
		return []string{obj.(*v1.Account).Spec.APIKeySecret.Name}
	}); err != nil {
		r.Log.Error(err, "failed to set cache index")
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named("odh-nim-controller").
		For(&v1.Account{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&corev1.Secret{}).
		Owns(&templatev1.Template{}).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(
			func(ctx context.Context, obj client.Object) []reconcile.Request {
				var requests []reconcile.Request
				accounts := &v1.AccountList{}
				if err := mgr.GetClient().List(ctx, accounts, client.MatchingFields{apiKeySpecPath: obj.GetName()}); err != nil {
					r.Log.Error(err, "failed to fetch accounts")
					return requests
				}
				for _, item := range accounts.Items {
					requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{
						Name:      item.Name,
						Namespace: item.Namespace,
					}})
				}
				return requests
			})).
		Complete(r)
}

// TODO Reconcile daily

func (r *NimAccountReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	account := &v1.Account{}
	if err := r.Client.Get(ctx, req.NamespacedName, account); err != nil {
		if errors.IsNotFound(err) {
			// we clean up deleted accounts using finalizers, nothing to do here
			r.Log.V(1).Info("account deleted")
		} else {
			r.Log.V(1).Error(err, "failed to fetch object")
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	accountStatus := metav1.ConditionFalse        // is modified for overall success
	accountStatusReason := "AccountNotSuccessful" // is modified for every failure or overall success
	defer func() {
		if err := r.updateStatusCondition(ctx, account, condAccountStatus, accountStatus, accountStatusReason); err != nil {
			r.Log.Error(err, "failed to update account status condition")
		}
	}()

	if account.DeletionTimestamp.IsZero() {
		if !controllerutil.ContainsFinalizer(account, constants.FinalizerNimAccount) {
			if err := patchutils.JsonPatchFinalizerInQ(ctx, r.Client, account, constants.FinalizerNimAccount)(); err != nil {
				r.Log.V(1).Error(err, "account active, failed to add finalizer")
				return ctrl.Result{}, err
			}
		}
		r.Log.V(1).Info("account active, finalizer exists")
	} else {
		if controllerutil.ContainsFinalizer(account, constants.FinalizerNimAccount) {
			// TODO account being deleted, cleanups?

			if err := patchutils.JsonPatchFinalizerOutQ(ctx, r.Client, account, constants.FinalizerNimAccount)(); err != nil {
				r.Log.V(1).Error(err, "account being deleted, cleanups done, failed to remove finalizer")
				return ctrl.Result{}, err
			}
		}
		r.Log.V(1).Info("account being deleted, cleanups done, finalizer removed")
		return ctrl.Result{}, nil
	}

	apiKeySecret := &corev1.Secret{}
	apiKeySecretSubject := types.NamespacedName{Name: account.Spec.APIKeySecret.Name, Namespace: account.Namespace}
	if err := r.Client.Get(ctx, apiKeySecretSubject, apiKeySecret); err != nil {
		if errors.IsNotFound(err) {
			r.Log.V(1).Info("api key secret was deleted")
			// TODO api key secret was deleted, cleanups?
		} else {
			r.Log.V(1).Error(err, "failed to fetch api key secret")
		}
		accountStatusReason = "ApiKeySecretNotAvailable"
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	apiKeyBytes, foundKey := apiKeySecret.Data["api_key"]
	if !foundKey {
		err := fmt.Errorf("secret %+v has no api_key data", apiKeySecretSubject)
		r.Log.V(1).Error(err, "failed to find api key data in secret")
		accountStatusReason = "NoApiKeyInSecret"
		return ctrl.Result{}, err
	}

	r.Log.V(1).Info("got api key")
	apiKeyStr := string(apiKeyBytes)

	availableImages, imagesErr := utils.GetAvailableNimImageList()
	if imagesErr != nil {
		r.Log.V(1).Error(imagesErr, "failed to fetch NIM available custom runtimes")
		return ctrl.Result{}, imagesErr
	}

	// validate api key
	apiKeyStatus := metav1.ConditionTrue
	apiKeyStatusReason := "ApiKeyValidated"
	if err := utils.ValidateApiKey(apiKeyStr, availableImages[0]); err != nil {
		apiKeyStatus = metav1.ConditionFalse
		apiKeyStatusReason = "ApiKeyNotValidated"
		accountStatusReason = apiKeyStatusReason
	}
	if err := r.updateStatusCondition(ctx, account, condAPIKeyValidation, apiKeyStatus, apiKeyStatusReason); err != nil {
		r.Log.Error(err, "failed to update account status condition")
	}

	// update configmap
	nimConfigStatus := metav1.ConditionTrue
	nimConfigStatusReason := "ConfigMapUpdated"
	if err := r.reconcileNimConfig(ctx, req.Namespace); err != nil {
		nimConfigStatus = metav1.ConditionFalse
		nimConfigStatusReason = "ConfigMapNotUpdated"
		accountStatusReason = nimConfigStatusReason
	}
	if err := r.updateStatusCondition(ctx, account, condConfigMapUpdate, nimConfigStatus, nimConfigStatusReason); err != nil {
		r.Log.Error(err, "failed to update account status condition")
	}

	// update template
	templateStatus := metav1.ConditionTrue
	templateStatusReason := "TemplateUpdated"
	if err := r.reconcileRuntimeTemplate(ctx, req.Namespace); err != nil {
		templateStatus = metav1.ConditionFalse
		templateStatusReason = "TemplateNotUpdated"
		accountStatusReason = templateStatusReason
	}
	if err := r.updateStatusCondition(ctx, account, condTemplateUpdate, templateStatus, templateStatusReason); err != nil {
		r.Log.Error(err, "failed to update account status condition")
	}

	// update pull secret
	pullSecretStatus := metav1.ConditionTrue
	pullSecretStatusReason := "SecretUpdated"
	if err := r.reconcileNimPullSecret(ctx, req.Namespace, apiKeyStr); err != nil {
		pullSecretStatus = metav1.ConditionFalse
		pullSecretStatusReason = "SecretNotUpdated"
		accountStatusReason = pullSecretStatusReason
	}
	if err := r.updateStatusCondition(ctx, account, condSecretUpdate, pullSecretStatus, pullSecretStatusReason); err != nil {
		r.Log.Error(err, "failed to update account status condition")
	}

	// if reached here, account was successful, deferred func will update the status
	accountStatus = metav1.ConditionTrue
	accountStatusReason = "AccountSuccessful"

	return ctrl.Result{}, nil
}

func (r *NimAccountReconciler) reconcileNimConfig(ctx context.Context, namespace string) error {
	// TODO WIP

	cmap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "nvidia-nim-data",
			Namespace: namespace,
		},
	}

	if _, err := controllerutil.CreateOrPatch(ctx, r.Client, cmap, func() error {
		cmap.Data = nil // TODO
		return nil
	}); err != nil {
		return err
	}

	return nil
}

func (r *NimAccountReconciler) reconcileRuntimeTemplate(ctx context.Context, namespace string) error {
	// TODO code
	return nil
}

func (r *NimAccountReconciler) reconcileNimPullSecret(ctx context.Context, namespace, apiKey string) error {
	// TODO code
	return nil
}

func (r *NimAccountReconciler) updateStatusCondition(ctx context.Context, account *v1.Account, conditionType conditionType, status metav1.ConditionStatus, reason string) error {
	// TODO code
	return nil
}
