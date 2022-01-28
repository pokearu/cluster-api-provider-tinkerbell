/*
Copyright 2020 The Kubernetes Authors.

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
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/cluster-api/util/patch"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	infrastructurev1 "github.com/tinkerbell/cluster-api-provider-tinkerbell/api/v1beta1"
	pbnjv1 "github.com/tinkerbell/cluster-api-provider-tinkerbell/pbnj/api/v1alpha1"
	tinkv1 "github.com/tinkerbell/cluster-api-provider-tinkerbell/tink/api/v1alpha1"
)

// ReconcileContext describes functionality required for reconciling Machine or Cluster object
// into Tinkerbell Kubernetes node.
type ReconcileContext interface {
	Reconcile() error
}

// baseMachineReconcileContext contains enough information to decide if given machine should
// be removed or created.
type baseMachineReconcileContext struct {
	log               logr.Logger
	ctx               context.Context
	tinkerbellMachine *infrastructurev1.TinkerbellMachine
	patchHelper       *patch.Helper
	client            client.Client
}

// BaseMachineReconcileContext is an interface allowing basic machine reconciliation which
// involves either object removal or further processing using MachineReconcileContext interface.
type BaseMachineReconcileContext interface {
	MachineScheduledForDeletion() bool
	DeleteMachineWithDependencies() error
	IntoMachineReconcileContext() (ReconcileContext, error)
	Log() logr.Logger
}

var (
	// ErrMachineVersionEmpty is the error returned when Version is not set on the parent Machine.
	ErrMachineVersionEmpty = fmt.Errorf("machine version is empty")
	// ErrConfigurationNil is the error returned when TinkerbellMachineReconciler or TinkerbellClusterReconciler is nil.
	ErrConfigurationNil = fmt.Errorf("configuration is nil")
	// ErrMissingClient is the error returned when TinkerbellMachineReconciler or TinkerbellClusterReconciler do
	// not have a Client configured.
	ErrMissingClient = fmt.Errorf("client is nil")
	// ErrMissingBootstrapDataSecretValueKey is the error returned when the Secret referenced for bootstrap data
	// is missing the value key.
	ErrMissingBootstrapDataSecretValueKey = fmt.Errorf("retrieving bootstrap data: secret value key is missing")
	// ErrBootstrapUserDataEmpty is the error returned when the referenced bootstrap data is empty.
	ErrBootstrapUserDataEmpty = fmt.Errorf("received bootstrap user data is empty")
)

// New builds a context for machine reconciliation process, collecting all required
// information.
//
// If unexpected case occurs, error is returned.
//
// If some data is not yet available, nil is returned.
//
//nolint:lll
func (tmr *TinkerbellMachineReconciler) newReconcileContext(ctx context.Context, namespacedName types.NamespacedName) (BaseMachineReconcileContext, ctrl.Result, error) {
	if err := tmr.validate(); err != nil {
		return nil, ctrl.Result{}, fmt.Errorf("invalid configuration: %w", err)
	}

	log := ctrl.LoggerFrom(ctx)

	bmrc := &baseMachineReconcileContext{
		log:               log.WithValues("TinkerbellMachine", namespacedName),
		ctx:               ctx,
		tinkerbellMachine: &infrastructurev1.TinkerbellMachine{},
		client:            tmr.Client,
	}

	if err := bmrc.client.Get(bmrc.ctx, namespacedName, bmrc.tinkerbellMachine); err != nil {
		if apierrors.IsNotFound(err) {
			bmrc.log.Info("TinkerbellMachine not found")

			return nil, ctrl.Result{}, nil
		}

		return nil, ctrl.Result{}, fmt.Errorf("getting TinkerbellMachine: %w", err)
	}

	patchHelper, err := patch.NewHelper(bmrc.tinkerbellMachine, bmrc.client)
	if err != nil {
		return nil, ctrl.Result{}, fmt.Errorf("initializing patch helper: %w", err)
	}

	bmrc.patchHelper = patchHelper

	return bmrc, ctrl.Result{}, nil
}

// validate validates if context configuration has all required fields properly populated.
func (tmr *TinkerbellMachineReconciler) validate() error {
	if tmr == nil {
		return ErrConfigurationNil
	}

	if tmr.Client == nil {
		return ErrMissingClient
	}

	return nil
}

// MachineScheduledForDeletion implements BaseMachineReconcileContext interface method
// using TinkerbellMachine deletion timestamp.
func (bmrc *baseMachineReconcileContext) MachineScheduledForDeletion() bool {
	return !bmrc.tinkerbellMachine.ObjectMeta.DeletionTimestamp.IsZero()
}

func (bmrc *baseMachineReconcileContext) releaseHardware() error {
	hardware := &tinkv1.Hardware{}

	namespacedName := types.NamespacedName{
		Name: bmrc.tinkerbellMachine.Spec.HardwareName,
	}

	if err := bmrc.client.Get(bmrc.ctx, namespacedName, hardware); err != nil {
		return fmt.Errorf("getting hardware: %w", err)
	}

	patchHelper, err := patch.NewHelper(hardware, bmrc.client)
	if err != nil {
		return fmt.Errorf("initializing patch helper for selected hardware: %w", err)
	}

	delete(hardware.ObjectMeta.Labels, HardwareOwnerNameLabel)
	delete(hardware.ObjectMeta.Labels, HardwareOwnerNamespaceLabel)

	controllerutil.RemoveFinalizer(hardware, infrastructurev1.MachineFinalizer)

	if err := patchHelper.Patch(bmrc.ctx, hardware); err != nil {
		return fmt.Errorf("patching Hardware object: %w", err)
	}

	return bmrc.deprovisionHardware(hardware)
}

func (bmrc *baseMachineReconcileContext) deprovisionHardware(hardware *tinkv1.Hardware) error {
	if hardware.Spec.BmcRef == "" {
		bmrc.log.Info("Skipping deprovision for hardware", "BMC Ref", hardware.Spec.BmcRef, "Hardware name", hardware.Name)

		return nil
	}

	// Fetch the bmc.
	bmc := &pbnjv1.BMC{}
	namespacedName := types.NamespacedName{
		Name: hardware.Spec.BmcRef,
	}

	if err := bmrc.client.Get(bmrc.ctx, namespacedName, bmc); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("BMC not found: %w", err)
		}

		return fmt.Errorf("failed to get bmc: %w", err)
	}

	patchHelper, err := patch.NewHelper(bmc, bmrc.client)
	if err != nil {
		return fmt.Errorf("initializing patch helper for bmc: %w", err)
	}

	bmc.Spec.BootDevice = "BOOT_DEVICE_PXE"
	bmc.Spec.PowerAction = "POWER_ACTION_HARDOFF"

	if err := patchHelper.Patch(bmrc.ctx, bmc); err != nil {
		return fmt.Errorf("patching BMC object: %w", err)
	}

	return nil
}

// DeleteMachineWithDependencies removes template and workflow objects associated with given machine.
func (bmrc *baseMachineReconcileContext) DeleteMachineWithDependencies() error {
	bmrc.log.Info("Removing machine", "hardwareName", bmrc.tinkerbellMachine.Spec.HardwareName)

	if err := bmrc.removeTemplate(); err != nil {
		return fmt.Errorf("removing Template: %w", err)
	}

	if err := bmrc.removeWorkflow(); err != nil {
		return fmt.Errorf("removing Workflow: %w", err)
	}

	if err := bmrc.releaseHardware(); err != nil {
		return fmt.Errorf("releasing Hardware: %w", err)
	}

	controllerutil.RemoveFinalizer(bmrc.tinkerbellMachine, infrastructurev1.MachineFinalizer)

	bmrc.log.Info("Patching Machine object to remove finalizer")

	return bmrc.patch()
}

// IntoMachineReconcileContext implements BaseMachineReconcileContext by building MachineReconcileContext
// from existing fields.
func (bmrc *baseMachineReconcileContext) IntoMachineReconcileContext() (ReconcileContext, error) {
	machine, err := bmrc.getReadyMachine()
	if err != nil {
		return nil, fmt.Errorf("getting valid Machine object: %w", err)
	}

	if machine == nil {
		return nil, nil
	}

	bootstrapCloudConfig, err := bmrc.getReadyBootstrapCloudConfig(machine)
	if err != nil {
		return nil, fmt.Errorf("receiving bootstrap cloud config: %w", err)
	}

	tinkerbellCluster, err := bmrc.getReadyTinkerbellCluster(machine)
	if err != nil {
		return nil, fmt.Errorf("getting TinkerbellCluster: %w", err)
	}

	if tinkerbellCluster == nil {
		bmrc.log.Info("TinkerbellCluster is not ready yet")

		return nil, nil
	}

	return &machineReconcileContext{
		baseMachineReconcileContext: bmrc,
		machine:                     machine,
		tinkerbellCluster:           tinkerbellCluster,
		bootstrapCloudConfig:        bootstrapCloudConfig,
	}, nil
}

// Log implements BaseMachineReconcileContext by returning internal logger, which is enhanced with
// context information which has already been fetched.
func (bmrc *baseMachineReconcileContext) Log() logr.Logger {
	return bmrc.log
}

// removeTemplate makes sure template for TinkerbellMachine has been cleaned up.
func (bmrc *baseMachineReconcileContext) removeTemplate() error {
	namespacedName := types.NamespacedName{
		Name:      bmrc.tinkerbellMachine.Name,
		Namespace: bmrc.tinkerbellMachine.Namespace,
	}

	template := &tinkv1.Template{}

	err := bmrc.client.Get(bmrc.ctx, namespacedName, template)
	if err != nil {
		if apierrors.IsNotFound(err) {
			bmrc.log.Info("Template already removed", "name", namespacedName)

			return nil
		}

		return fmt.Errorf("checking if template exists: %w", err)
	}

	bmrc.log.Info("Removing Template", "name", namespacedName)

	if err := bmrc.client.Delete(bmrc.ctx, template); err != nil {
		return fmt.Errorf("ensuring template has been removed: %w", err)
	}

	return nil
}

// removeWorkflow makes sure workflow for TinkerbellMachine has been cleaned up.
func (bmrc *baseMachineReconcileContext) removeWorkflow() error {
	namespacedName := types.NamespacedName{
		Name:      bmrc.tinkerbellMachine.Name,
		Namespace: bmrc.tinkerbellMachine.Namespace,
	}

	workflow := &tinkv1.Workflow{}

	err := bmrc.client.Get(bmrc.ctx, namespacedName, workflow)
	if err != nil {
		if apierrors.IsNotFound(err) {
			bmrc.log.Info("Workflow already removed", "name", namespacedName)

			return nil
		}

		return fmt.Errorf("checking if workflow exists: %w", err)
	}

	bmrc.log.Info("Removing Workflow", "name", namespacedName)

	if err := bmrc.client.Delete(bmrc.ctx, workflow); err != nil {
		return fmt.Errorf("ensuring workflow has been removed: %w", err)
	}

	return nil
}

// patch commits all done changes to TinkerbellMachine object. If patching fails, error
// is returned.
func (bmrc *baseMachineReconcileContext) patch() error {
	// TODO: Improve control on when to patch the object.
	if err := bmrc.patchHelper.Patch(bmrc.ctx, bmrc.tinkerbellMachine); err != nil {
		return fmt.Errorf("patching machine object: %w", err)
	}

	return nil
}

// getReadyMachine returns valid ClusterAPI Machine object.
//
// If error occurs while fetching the machine, error is returned.
//
// If machine is not ready yet, nil is returned.
func (bmrc *baseMachineReconcileContext) getReadyMachine() (*clusterv1.Machine, error) {
	// Continue building the context with some validation rules.
	machine, err := util.GetOwnerMachine(bmrc.ctx, bmrc.client, bmrc.tinkerbellMachine.ObjectMeta)
	if err != nil {
		return nil, fmt.Errorf("getting Machine object: %w", err)
	}

	reason, err := isMachineReady(machine)
	if err != nil {
		return nil, fmt.Errorf("validating Machine object: %w", err)
	}

	if reason != "" {
		bmrc.log.Info("machine is not ready yet", "reason", reason)

		return nil, nil
	}

	return machine, nil
}

// isMachineReady validates that given Machine object is ready for further processing.
//
// If machine is not ready, string reason is returned.
//
// If machine is ready, empty string is returned.
func isMachineReady(machine *clusterv1.Machine) (string, error) {
	if machine == nil {
		return "Machine Controller has not yet set OwnerRef", nil
	}

	if machine.Spec.Bootstrap.DataSecretName == nil {
		return "retrieving bootstrap data: linked Machine's bootstrap.dataSecretName is not available yet", nil
	}

	// Spec says this field is optional, but @detiber says it's effectively required,
	// so treat it as so.
	if machine.Spec.Version == nil || *machine.Spec.Version == "" {
		return "", ErrMachineVersionEmpty
	}

	return "", nil
}

// getReadyBootstrapCloudConfig returns initialized bootstrap cloud config for a given machine.
//
// If bootstrap cloud config is not yet initialized, empty string is returned.
func (bmrc *baseMachineReconcileContext) getReadyBootstrapCloudConfig(machine *clusterv1.Machine) (string, error) {
	secret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: machine.Namespace, Name: *machine.Spec.Bootstrap.DataSecretName}

	if err := bmrc.client.Get(bmrc.ctx, key, secret); err != nil {
		return "", fmt.Errorf("retrieving bootstrap data secret: %w", err)
	}

	bootstrapUserData, ok := secret.Data["value"]
	if !ok {
		return "", ErrMissingBootstrapDataSecretValueKey
	}

	if len(bootstrapUserData) == 0 {
		return "", ErrBootstrapUserDataEmpty
	}

	return string(bootstrapUserData), nil
}

// getTinkerbellCluster returns associated TinkerbellCluster object for a given machine.
func (bmrc *baseMachineReconcileContext) getReadyTinkerbellCluster(machine *clusterv1.Machine) (*infrastructurev1.TinkerbellCluster, error) { //nolint:lll
	cluster, err := util.GetClusterFromMetadata(bmrc.ctx, bmrc.client, machine.ObjectMeta)
	if err != nil {
		return nil, fmt.Errorf("getting cluster from metadata: %w", err)
	}

	tinkerbellCluster := &infrastructurev1.TinkerbellCluster{}
	tinkerbellClusterNamespacedName := client.ObjectKey{
		Namespace: bmrc.tinkerbellMachine.Namespace,
		Name:      cluster.Spec.InfrastructureRef.Name,
	}

	if err := bmrc.client.Get(bmrc.ctx, tinkerbellClusterNamespacedName, tinkerbellCluster); err != nil {
		return nil, fmt.Errorf("getting TinkerbellCluster object: %w", err)
	}

	if !tinkerbellCluster.Status.Ready {
		bmrc.log.Info("cluster not ready yet")

		return nil, nil
	}

	return tinkerbellCluster, nil
}
