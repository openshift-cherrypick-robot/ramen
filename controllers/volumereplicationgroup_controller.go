/*
Copyright 2021 The RamenDR authors.

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
	"reflect"

	"github.com/go-logr/logr"

	volrep "github.com/csi-addons/volume-replication-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlcontroller "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	ramendrv1alpha1 "github.com/ramendr/ramen/api/v1alpha1"
	rmnutil "github.com/ramendr/ramen/controllers/util"
	"github.com/ramendr/ramen/controllers/volsync"
)

type PVDownloader interface {
	DownloadPVs(objStore ObjectStorer, s3KeyPrefix string) ([]corev1.PersistentVolume, error)
}

type PVUploader interface {
	UploadPV(objectStore ObjectStorer, pvKeyPrefix string, pv *corev1.PersistentVolume) error
}

type PVDeleter interface {
	DeletePVs(v interface{}, s3ProfileName string) error
}

// VolumeReplicationGroupReconciler reconciles a VolumeReplicationGroup object
type VolumeReplicationGroupReconciler struct {
	client.Client
	APIReader      client.Reader
	Log            logr.Logger
	PVDownloader   PVDownloader
	PVUploader     PVUploader
	PVDeleter      PVDeleter
	ObjStoreGetter ObjectStoreGetter
	Scheme         *runtime.Scheme
	eventRecorder  *rmnutil.EventReporter
}

// SetupWithManager sets up the controller with the Manager.
func (r *VolumeReplicationGroupReconciler) SetupWithManager(mgr ctrl.Manager) error {
	pvcPredicate := pvcPredicateFunc()
	pvcMapFun := handler.EnqueueRequestsFromMapFunc(handler.MapFunc(func(obj client.Object) []reconcile.Request {
		log := ctrl.Log.WithName("pvcmap").WithName("VolumeReplicationGroup")

		pvc, ok := obj.(*corev1.PersistentVolumeClaim)
		if !ok {
			log.Info("PersistentVolumeClaim(PVC) map function received non-PVC resource")

			return []reconcile.Request{}
		}

		return filterPVC(mgr, pvc,
			log.WithValues("pvc", types.NamespacedName{Name: pvc.Name, Namespace: pvc.Namespace}))
	}))

	r.eventRecorder = rmnutil.NewEventReporter(mgr.GetEventRecorderFor("controller_VolumeReplicationGroup"))

	r.Log.Info("Adding VolumeReplicationGroup controller")

	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(ctrlcontroller.Options{MaxConcurrentReconciles: getMaxConcurrentReconciles(r.Log)}).
		For(&ramendrv1alpha1.VolumeReplicationGroup{}).
		Watches(&source.Kind{Type: &corev1.PersistentVolumeClaim{}}, pvcMapFun, builder.WithPredicates(pvcPredicate)).
		Owns(&volrep.VolumeReplication{}).
		Complete(r)
}

func init() {
	// Register custom metrics with the global Prometheus registry here
}

// pvcPredicateFunc sends reconcile requests for create and delete events.
// For them the filtering of whether the pvc belongs to the any of the
// VolumeReplicationGroup CRs and identifying such a CR is done in the
// map function by comparing namespaces and labels.
// But for update of pvc, the reconcile request should be sent only for
// specific changes. Do that comparison here.
func pvcPredicateFunc() predicate.Funcs {
	pvcPredicate := predicate.Funcs{
		// NOTE: Create predicate is retained, to help with logging the event
		CreateFunc: func(e event.CreateEvent) bool {
			log := ctrl.Log.WithName("pvcmap").WithName("VolumeReplicationGroup")

			log.Info("Create event for PersistentVolumeClaim")

			return true
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			log := ctrl.Log.WithName("pvcmap").WithName("VolumeReplicationGroup")
			oldPVC, ok := e.ObjectOld.DeepCopyObject().(*corev1.PersistentVolumeClaim)
			if !ok {
				log.Info("Failed to deep copy older PersistentVolumeClaim")

				return false
			}
			newPVC, ok := e.ObjectNew.DeepCopyObject().(*corev1.PersistentVolumeClaim)
			if !ok {
				log.Info("Failed to deep copy newer PersistentVolumeClaim")

				return false
			}

			log.Info("Update event for PersistentVolumeClaim")

			return updateEventDecision(oldPVC, newPVC, log)
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			// PVC deletion is held back till VRG deletion. This is to
			// avoid races between subscription deletion and updating
			// VRG state. If VRG state is not updated prior to subscription
			// cleanup, then PVC deletion (triggered by subscription
			// cleanup) would leaving behind VolRep resource with stale
			// state (as per the current VRG state).
			return false
		},
	}

	return pvcPredicate
}

func updateEventDecision(oldPVC *corev1.PersistentVolumeClaim,
	newPVC *corev1.PersistentVolumeClaim,
	log logr.Logger) bool {
	const requeue bool = true

	pvcNamespacedName := types.NamespacedName{Name: newPVC.Name, Namespace: newPVC.Namespace}
	predicateLog := log.WithValues("pvc", pvcNamespacedName.String())
	// If finalizers change then deep equal of spec fails to catch it, we may want more
	// conditions here, compare finalizers and also status.phase to catch bound PVCs
	if !reflect.DeepEqual(oldPVC.Spec, newPVC.Spec) {
		predicateLog.Info("Reconciling due to change in spec")

		return requeue
	}

	if oldPVC.Status.Phase != corev1.ClaimBound && newPVC.Status.Phase == corev1.ClaimBound {
		predicateLog.Info("Reconciling due to phase change", "oldPhase", oldPVC.Status.Phase,
			"newPhase", newPVC.Status.Phase)

		return requeue
	}

	// This check may not be needed and can lead to some
	// unnecessary reconciles being triggered when the
	// pod that uses this pvc gets rescheduled to some
	// other node and pvcInUse finalizer is removed as
	// no pod is mounting it.
	if containsString(oldPVC.ObjectMeta.Finalizers, pvcInUse) &&
		!containsString(newPVC.ObjectMeta.Finalizers, pvcInUse) {
		predicateLog.Info("Reconciling due to pvc not in use")

		return requeue
	}

	// If newPVC is not yet bound, dont requeue.
	// If the newPVC is being deleted and VR protection finalizer is
	// not there, then dont requeue.
	// skipResult false means, the above conditions are not met.
	if skipResult, _ := skipPVC(newPVC, predicateLog); !skipResult {
		predicateLog.Info("Reconciling due to VR Protection finalizer")

		return requeue
	}

	predicateLog.Info("Not Requeuing", "oldPVC Phase", oldPVC.Status.Phase,
		"newPVC phase", newPVC.Status.Phase)

	return !requeue
}

func filterPVC(mgr manager.Manager, pvc *corev1.PersistentVolumeClaim, log logr.Logger) []reconcile.Request {
	req := []reconcile.Request{}

	var vrgs ramendrv1alpha1.VolumeReplicationGroupList

	listOptions := []client.ListOption{
		client.InNamespace(pvc.Namespace),
	}

	// decide if reconcile request needs to be sent to the
	// corresponding VolumeReplicationGroup CR by:
	// - whether there is a VolumeReplicationGroup CR in the namespace
	//   to which the the pvc belongs to.
	// - whether the labels on pvc match the label selectors from
	//    VolumeReplicationGroup CR.
	err := mgr.GetClient().List(context.TODO(), &vrgs, listOptions...)
	if err != nil {
		log.Error(err, "Failed to get list of VolumeReplicationGroup resources")

		return []reconcile.Request{}
	}

	for _, vrg := range vrgs.Items {
		vrgLabelSelector := vrg.Spec.PVCSelector
		selector, err := metav1.LabelSelectorAsSelector(&vrgLabelSelector)
		// continue if we fail to get the labels for this object hoping
		// that pvc might actually belong to  some other vrg instead of
		// this. If not found, then reconcile request would not be sent
		if err != nil {
			log.Error(err, "Failed to get the label selector from VolumeReplicationGroup", "vrgName", vrg.Name)

			continue
		}

		if selector.Matches(labels.Set(pvc.GetLabels())) {
			log.Info("Found VolumeReplicationGroup with matching labels",
				"vrg", vrg.Name, "labeled", selector)

			req = append(req, reconcile.Request{NamespacedName: types.NamespacedName{Name: vrg.Name, Namespace: vrg.Namespace}})
		}
	}

	return req
}

// nolint: lll // disabling line length linter
// +kubebuilder:rbac:groups=ramendr.openshift.io,resources=volumereplicationgroups,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=ramendr.openshift.io,resources=volumereplicationgroups/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=ramendr.openshift.io,resources=volumereplicationgroups/finalizers,verbs=update
// +kubebuilder:rbac:groups=replication.storage.openshift.io,resources=volumereplications,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=replication.storage.openshift.io,resources=volumereplicationclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=storage.k8s.io,resources=storageclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=persistentvolumeclaims,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=core,resources=persistentvolumes,verbs=get;list;watch;update;patch;create
// +kubebuilder:rbac:groups=core,resources=events,verbs=get;create;patch;update
// +kubebuilder:rbac:groups="",namespace=system,resources=secrets,verbs=get

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the VolumeReplicationGroup object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.7.0/pkg/reconcile
func (r *VolumeReplicationGroupReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("VolumeReplicationGroup", req.NamespacedName)

	log.Info("Entering reconcile loop")

	defer log.Info("Exiting reconcile loop")

	v := VRGInstance{
		reconciler:     r,
		ctx:            ctx,
		log:            log,
		instance:       &ramendrv1alpha1.VolumeReplicationGroup{},
		volRepPVCs:     []corev1.PersistentVolumeClaim{},
		volSyncPVCs:    []corev1.PersistentVolumeClaim{},
		replClassList:  &volrep.VolumeReplicationClassList{},
		namespacedName: req.NamespacedName.String(),
	}

	// Fetch the VolumeReplicationGroup instance
	if err := r.APIReader.Get(ctx, req.NamespacedName, v.instance); err != nil {
		if errors.IsNotFound(err) {
			log.Info("Resource not found")

			return ctrl.Result{}, nil
		}

		log.Error(err, "Failed to get resource")

		return ctrl.Result{}, fmt.Errorf("failed to reconcile VolumeReplicationGroup (%v), %w",
			req.NamespacedName, err)
	}

	v.volSyncHandler = volsync.NewVSHandler(
		ctx, r.Client, log, v.instance, v.instance.Spec.Async.SchedulingInterval,
		&ramendrv1alpha1.VolSyncProfile{
			VolSyncProfileName: "default",
			ServiceType:        &volsync.DefaultRsyncServiceType,
		})

	// Save a copy of the instance status to be used for the VRG status update comparison
	v.instance.Status.DeepCopyInto(&v.savedInstanceStatus)

	if v.savedInstanceStatus.ProtectedPVCs == nil {
		v.savedInstanceStatus.ProtectedPVCs = []ramendrv1alpha1.ProtectedPVC{}
	}

	res, err := v.processVRG()
	log.Info(fmt.Sprintf("VolRep count %d, VolSync count %d", len(v.volRepPVCs), len(v.volSyncPVCs)))

	return res, err
}

type VRGInstance struct {
	reconciler          *VolumeReplicationGroupReconciler
	ctx                 context.Context
	log                 logr.Logger
	instance            *ramendrv1alpha1.VolumeReplicationGroup
	savedInstanceStatus ramendrv1alpha1.VolumeReplicationGroupStatus
	volRepPVCs          []corev1.PersistentVolumeClaim
	volSyncPVCs         []corev1.PersistentVolumeClaim
	replClassList       *volrep.VolumeReplicationClassList
	vrcUpdated          bool
	namespacedName      string
	volSyncHandler      *volsync.VSHandler
}

const (
	// Finalizers
	vrgFinalizerName        = "volumereplicationgroups.ramendr.openshift.io/vrg-protection"
	pvcVRFinalizerProtected = "volumereplicationgroups.ramendr.openshift.io/pvc-vr-protection"
	pvcInUse                = "kubernetes.io/pvc-protection"

	// Annotations
	pvcVRAnnotationProtectedKey   = "volumereplicationgroups.ramendr.openshift.io/vr-protected"
	pvcVRAnnotationProtectedValue = "protected"
	pvVRAnnotationRetentionKey    = "volumereplicationgroups.ramendr.openshift.io/vr-retained"
	pvVRAnnotationRetentionValue  = "retained"
	PVRestoreAnnotation           = "volumereplicationgroups.ramendr.openshift.io/ramen-restore"
)

func (v *VRGInstance) processVRG() (ctrl.Result, error) {
	v.initializeStatus()

	if err := v.validateVRGState(); err != nil {
		// record the event
		v.log.Error(err, "Failed to validate the spec state")
		rmnutil.ReportIfNotPresent(v.reconciler.eventRecorder, v.instance, corev1.EventTypeWarning,
			rmnutil.EventReasonValidationFailed, err.Error())

		msg := "VolumeReplicationGroup state is invalid"
		setVRGDataErrorCondition(&v.instance.Status.Conditions, v.instance.Generation, msg)

		if err = v.updateVRGStatus(false); err != nil {
			v.log.Error(err, "Status update failed")
			// Since updating status failed, reconcile
			return ctrl.Result{Requeue: true}, nil
		}
		// No requeue, as there is no reconcile till user changes desired spec to a valid value
		return ctrl.Result{}, nil
	}

	// If neither of Async or Sync mode is provided, then
	// dont requeue. Just return error.
	if err := v.validateVRGMode(); err != nil {
		// record the event
		v.log.Error(err, "Failed to validate the spec mode")
		rmnutil.ReportIfNotPresent(v.reconciler.eventRecorder, v.instance, corev1.EventTypeWarning,
			rmnutil.EventReasonValidationFailed, err.Error())

		msg := "VolumeReplicationGroup mode is invalid"
		setVRGDataErrorCondition(&v.instance.Status.Conditions, v.instance.Generation, msg)

		if err = v.updateVRGStatus(false); err != nil {
			v.log.Error(err, "Status update failed")
			// Since updating status failed, reconcile
			return ctrl.Result{Requeue: true}, nil
		}
		// No requeue, as there is no reconcile till user changes desired spec to a valid value
		return ctrl.Result{}, nil
	}

	if err := v.updatePVCList(); err != nil {
		v.log.Error(err, "Failed to update PersistentVolumeClaims for resource")

		rmnutil.ReportIfNotPresent(v.reconciler.eventRecorder, v.instance, corev1.EventTypeWarning,
			rmnutil.EventReasonValidationFailed, err.Error())

		msg := "Failed to get list of pvcs"
		setVRGDataErrorCondition(&v.instance.Status.Conditions, v.instance.Generation, msg)

		if err = v.updateVRGStatus(false); err != nil {
			v.log.Error(err, "VRG Status update failed")
		}

		return ctrl.Result{Requeue: true}, nil
	}

	return v.processVRGActions()
}

func (v *VRGInstance) processVRGActions() (ctrl.Result, error) {
	v.log = v.log.WithName("vrginstance").WithValues("State", v.instance.Spec.ReplicationState)

	switch {
	case !v.instance.GetDeletionTimestamp().IsZero():
		v.log = v.log.WithValues("Finalize", true)

		return v.processForDeletion()
	case v.instance.Spec.ReplicationState == ramendrv1alpha1.Primary:
		return v.processAsPrimary()
	default: // Secondary, not primary and not deleted
		return v.processAsSecondary()
	}
}

func (v *VRGInstance) validateVRGState() error {
	if v.instance.Spec.ReplicationState != ramendrv1alpha1.Primary &&
		v.instance.Spec.ReplicationState != ramendrv1alpha1.Secondary {
		err := fmt.Errorf("invalid or unknown replication state detected (deleted %v, desired replicationState %v)",
			!v.instance.GetDeletionTimestamp().IsZero(),
			v.instance.Spec.ReplicationState)

		v.log.Error(err, "Invalid request detected")

		return err
	}

	return nil
}

// Expectation is that either the sync mode (for MetroDR)
// or the async mode (for RegionalDR) is enabled. If none of
// them is enabled, then return error.
// This needs more thought as this function is making a
// compulsion that either of sync or async mode should be there.
func (v *VRGInstance) validateVRGMode() error {
	async := false
	sync := false

	if v.instance.Spec.Async.Mode == ramendrv1alpha1.AsyncModeEnabled {
		async = true
	}

	if v.instance.Spec.Sync.Mode == ramendrv1alpha1.SyncModeEnabled {
		sync = true
	}

	if !sync && !async {
		err := fmt.Errorf("neither of sync or async mode is enabled (deleted %v)",
			!v.instance.GetDeletionTimestamp().IsZero())

		v.log.Error(err, "Invalid request detected")

		return err
	}

	return nil
}

func (v *VRGInstance) restorePVs() error {
	if err := v.restorePVsForVolSync(); err != nil {
		v.log.Info("VolSync PV restore failed")

		return fmt.Errorf("failed to restore PVs for VolSync")
	}

	return v.restorePVsForVolRep()
}

func (v *VRGInstance) initializeStatus() {
	// create ProtectedPVCs map for status
	if v.instance.Status.ProtectedPVCs == nil {
		v.instance.Status.ProtectedPVCs = []ramendrv1alpha1.ProtectedPVC{}

		// Set the VRG conditions to unknown as nothing is known at this point
		msg := "Initializing VolumeReplicationGroup"
		setVRGInitialCondition(&v.instance.Status.Conditions, v.instance.Generation, msg)
	}
}

// updatePVCList fetches and updates the PVC list to process for the current instance of VRG
func (v *VRGInstance) updatePVCList() error {
	labelSelector := v.instance.Spec.PVCSelector

	v.log.Info("Fetching PersistentVolumeClaims", "labeled", labels.Set(labelSelector.MatchLabels))
	listOptions := []client.ListOption{
		client.InNamespace(v.instance.Namespace),
		client.MatchingLabels(labelSelector.MatchLabels),
	}

	pvcList := &corev1.PersistentVolumeClaimList{}
	if err := v.reconciler.List(v.ctx, pvcList, listOptions...); err != nil {
		v.log.Error(err, "Failed to list PersistentVolumeClaims",
			"labeled", labels.Set(labelSelector.MatchLabels))

		return fmt.Errorf("failed to list PersistentVolumeClaims, %w", err)
	}

	v.log.Info(fmt.Sprintf("Found %d PVCs using matching lables %v", len(pvcList.Items), labelSelector.MatchLabels))
	if !v.vrcUpdated {
		if err := v.updateReplicationClassList(); err != nil {
			v.log.Error(err, "Failed to get VolumeReplicationClass list")

			return fmt.Errorf("failed to get VolumeReplicationClass list")
		}

		v.vrcUpdated = true
	}

	if !v.instance.GetDeletionTimestamp().IsZero() {
		v.separatePVCsUsingVRGStatus(pvcList)
		v.log.Info(fmt.Sprintf("Separated PVCs (%d) into VolRepPVCs (%d) and VolSyncPVCs (%d)",
			len(pvcList.Items), len(v.volRepPVCs), len(v.volSyncPVCs)))

		return nil
	}

	if len(v.replClassList.Items) == 0 {
		v.volSyncPVCs = make([]corev1.PersistentVolumeClaim, len(pvcList.Items))
		numCopied := copy(v.volSyncPVCs, pvcList.Items)
		v.log.Info("No VolumeReplicationClass available. Using all PVCs with VolSync", "pvcCount", numCopied)

		return nil
	}

	// Separate PVCs targeted for VolRep from PVCs targeted for VolSync
	return v.separatePVCsUsingStorageClassProvisioner(pvcList)
}

func (v *VRGInstance) updateReplicationClassList() error {
	labelSelector := v.instance.Spec.Async.ReplicationClassSelector

	v.log.Info("Fetching VolumeReplicationClass", "labeled", labels.Set(labelSelector.MatchLabels))
	listOptions := []client.ListOption{
		client.MatchingLabels(labelSelector.MatchLabels),
	}

	if err := v.reconciler.List(v.ctx, v.replClassList, listOptions...); err != nil {
		v.log.Error(err, "Failed to list Replication Classes",
			"labeled", labels.Set(labelSelector.MatchLabels))

		return fmt.Errorf("failed to list Replication Classes, %w", err)
	}

	v.log.Info("Number of Replication Classes", "count", len(v.replClassList.Items))

	return nil
}

func (v *VRGInstance) separatePVCsUsingVRGStatus(pvcList *corev1.PersistentVolumeClaimList) {
	for idx := range pvcList.Items {
		pvc := &pvcList.Items[idx]

		for _, protectedPVC := range v.instance.Status.ProtectedPVCs {
			if pvc.Name == protectedPVC.Name {
				if protectedPVC.ProtectedByVolSync {
					v.volSyncPVCs = append(v.volSyncPVCs, *pvc)
				} else {
					v.volRepPVCs = append(v.volRepPVCs, *pvc)
				}
			}
		}
	}
}

func (v *VRGInstance) separatePVCsUsingStorageClassProvisioner(pvcList *corev1.PersistentVolumeClaimList) error {
	for idx := range pvcList.Items {
		pvc := &pvcList.Items[idx]
		scName := pvc.Spec.StorageClassName

		storageClass := &storagev1.StorageClass{}
		if err := v.reconciler.Get(v.ctx, types.NamespacedName{Name: *scName}, storageClass); err != nil {
			v.log.Info(fmt.Sprintf("Failed to get the storageclass %s", *scName))

			return fmt.Errorf("failed to get the storageclass with name %s (%w)", *scName, err)
		}

		for _, replicationClass := range v.replClassList.Items {
			if storageClass.Provisioner != replicationClass.Spec.Provisioner {
				v.volSyncPVCs = append(v.volSyncPVCs, *pvc)
			} else {
				v.volRepPVCs = append(v.volRepPVCs, *pvc)
			}
		}
	}

	v.log.Info(fmt.Sprintf("Found %d PVCs targeted for VolRep and %d targeted for VolSync",
		len(v.volRepPVCs), len(v.volSyncPVCs)))

	return nil
}

// finalizeVRG cleans up managed resources and removes the VRG finalizer for resource deletion
func (v *VRGInstance) processForDeletion() (ctrl.Result, error) {
	v.log.Info("Entering processing VolumeReplicationGroup")

	defer v.log.Info("Exiting processing VolumeReplicationGroup")

	if !containsString(v.instance.ObjectMeta.Finalizers, vrgFinalizerName) {
		v.log.Info("Finalizer missing from resource", "finalizer", vrgFinalizerName)

		return ctrl.Result{}, nil
	}

	if v.deleteVRGHandleMode() {
		v.log.Info("Requeuing as reconciling VolumeReplication for deletion failed")

		return ctrl.Result{Requeue: true}, nil
	}

	if v.instance.Spec.ReplicationState == ramendrv1alpha1.Primary {
		if err := v.deleteClusterDataInS3Stores(v.log); err != nil {
			v.log.Info("Requeuing due to failure in deleting PV cluster data from S3 stores",
				"errorValue", err)

			return ctrl.Result{Requeue: true}, nil
		}
	}

	if err := v.removeFinalizer(vrgFinalizerName); err != nil {
		v.log.Info("Failed to remove finalizer", "finalizer", vrgFinalizerName, "errorValue", err)

		return ctrl.Result{Requeue: true}, nil
	}

	rmnutil.ReportIfNotPresent(v.reconciler.eventRecorder, v.instance, corev1.EventTypeNormal,
		rmnutil.EventReasonDeleteSuccess, "Deletion Success")

	return ctrl.Result{}, nil
}

// For now, async mode and sync mode can be enabled only in either or fashion
// and the function reconcileVRsForDeletion is capable of handling it for both.
// However, in the future, we may want to enable both modes at the same time
// and might call different functions for those modes. This function is in
// preparation of that need.
func (v *VRGInstance) deleteVRGHandleMode() bool {
	return v.reconcileVRsForDeletion()
}

// removeFinalizer removes VRG finalizer form the resource
func (v *VRGInstance) removeFinalizer(finalizer string) error {
	v.instance.ObjectMeta.Finalizers = removeString(v.instance.ObjectMeta.Finalizers, finalizer)
	if err := v.reconciler.Update(v.ctx, v.instance); err != nil {
		v.log.Error(err, "Failed to remove finalizer", "finalizer", finalizer)

		return fmt.Errorf("failed to remove finalizer from VolumeReplicationGroup resource (%s/%s), %w",
			v.instance.Namespace, v.instance.Name, err)
	}

	return nil
}

// processAsPrimary reconciles the current instance of VRG as primary
func (v *VRGInstance) processAsPrimary() (ctrl.Result, error) {
	v.log.Info("Entering processing VolumeReplicationGroup")

	defer v.log.Info("Exiting processing VolumeReplicationGroup")

	if err := v.addFinalizer(vrgFinalizerName); err != nil {
		v.log.Info("Failed to add finalizer", "finalizer", vrgFinalizerName, "errorValue", err)

		msg := "Failed to add finalizer to VolumeReplicationGroup"
		setVRGDataErrorCondition(&v.instance.Status.Conditions, v.instance.Generation, msg)

		if err = v.updateVRGStatus(false); err != nil {
			v.log.Error(err, "VRG Status update failed")
		}

		return ctrl.Result{Requeue: true}, nil
	}

	if err := v.restorePVs(); err != nil {
		v.log.Info("Restoring PVs failed", "Error", err.Error())

		msg := fmt.Sprintf("Failed to restore PVs (%v)", err.Error())
		setVRGClusterDataErrorCondition(&v.instance.Status.Conditions, v.instance.Generation, msg)

		if err = v.updateVRGStatus(false); err != nil {
			v.log.Error(err, "VRG Status update failed")
		}

		// Since updating status failed, reconcile
		return ctrl.Result{Requeue: true}, nil
	}

	requeue := v.handleVRGMode(ramendrv1alpha1.Primary)

	// If requeue is false, then VRG was successfully processed as primary.
	// Hence the event to be generated is Success of type normal.
	// Expectation is that, if something failed and requeue is true, then
	// appropriate event might have been captured at the time of failure.
	if !requeue {
		rmnutil.ReportIfNotPresent(v.reconciler.eventRecorder, v.instance, corev1.EventTypeNormal,
			rmnutil.EventReasonPrimarySuccess, "Primary Success")
	}

	if err := v.updateVRGStatus(true); err != nil {
		requeue = true
	}

	if requeue {
		v.log.Info("Requeuing resource")

		return ctrl.Result{Requeue: requeue}, nil
	}

	v.log.Info("Successfully processed vrg as primary")

	return ctrl.Result{}, nil
}

func (v *VRGInstance) reconcileAsPrimary() bool {
	if len(v.volSyncPVCs) != 0 && !v.reconcileVolSyncAsPrimary() {
		return true // requeue
	}

	return v.reconcileVolRepsAsPrimary()
}

// processAsSecondary reconciles the current instance of VRG as secondary
func (v *VRGInstance) processAsSecondary() (ctrl.Result, error) {
	v.log.Info("Entering processing VolumeReplicationGroup")

	defer v.log.Info("Exiting processing VolumeReplicationGroup")

	if err := v.addFinalizer(vrgFinalizerName); err != nil {
		v.log.Info("Failed to add finalizer", "finalizer", vrgFinalizerName, "errorValue", err)

		msg := "Failed to add finalizer to VolumeReplicationGroup"
		setVRGDataErrorCondition(&v.instance.Status.Conditions, v.instance.Generation, msg)

		if err = v.updateVRGStatus(false); err != nil {
			v.log.Error(err, "VRG Status update failed")
		}

		return ctrl.Result{Requeue: true}, nil
	}

	requeue := v.handleVRGMode(ramendrv1alpha1.Secondary)

	// If requeue is false, then VRG was successfully processed as Secondary.
	// Hence the event to be generated is Success of type normal.
	// Expectation is that, if something failed and requeue is true, then
	// appropriate event might have been captured at the time of failure.
	if !requeue {
		rmnutil.ReportIfNotPresent(v.reconciler.eventRecorder, v.instance, corev1.EventTypeNormal,
			rmnutil.EventReasonSecondarySuccess, "Secondary Success")
	}

	if err := v.updateVRGStatus(true); err != nil {
		requeue = true
	}

	if requeue {
		v.log.Info("Requeuing resource")

		return ctrl.Result{Requeue: requeue}, nil
	}

	v.log.Info("Successfully processed vrg as secondary")

	return ctrl.Result{}, nil
}

func (v *VRGInstance) reconcileAsSecondary() bool {
	if !v.reconcileVolSyncAsSecondary() {
		return true // requeue
	}

	return v.reconcileVolRepsAsSecondary()
}

// For now, async mode and sync mode can be enabled only in either or fashion
// and the functions reconcileVRsAsPrimary reconcileVRsAsSecondary are capable
// of handling it for both. However, in the future, we may want to enable both
// the modes at the same time and might call different functions for those
// modes. This function is in preparation of that need.
func (v *VRGInstance) handleVRGMode(state ramendrv1alpha1.ReplicationState) (result bool) {
	if state == ramendrv1alpha1.Primary {
		result = v.reconcileAsPrimary()
	}

	if state == ramendrv1alpha1.Secondary {
		result = v.reconcileAsSecondary()
	}

	return result
}

func (v *VRGInstance) markAllPVCsProtected() {
	v.log.Info("marking all pvc resources ready for use and protected")

	msg := "PVC in the VolumeReplicationGroup is ready for use"

	for idx := range v.volRepPVCs {
		pvc := &v.volRepPVCs[idx]

		// Each protected PVC condition in VRG status has the same name
		// as PVC. Use that.
		v.updatePVCDataReadyCondition(pvc.Name, VRGConditionReasonReady, msg)
		v.updatePVCDataProtectedCondition(pvc.Name, VRGConditionReasonReady, msg)
	}
}

func (v *VRGInstance) updateVRGStatus(updateConditions bool) error {
	v.log.Info("Updating VRG status")

	if updateConditions {
		v.updateVRGConditions()
	}

	v.updateStatusState()

	v.instance.Status.ObservedGeneration = v.instance.Generation

	if !reflect.DeepEqual(v.savedInstanceStatus, v.instance.Status) {
		v.instance.Status.LastUpdateTime = metav1.Now()
		if err := v.reconciler.Status().Update(v.ctx, v.instance); err != nil {
			v.log.Info(fmt.Sprintf("Failed to update VRG status (%s/%s/%v)",
				v.instance.Name, v.instance.Namespace, err))

			return fmt.Errorf("failed to update VRG status (%s/%s)", v.instance.Name, v.instance.Namespace)
		}

		v.log.Info(fmt.Sprintf("Updated VRG Status %+v", v.instance.Status))

		return nil
	}

	v.log.Info(fmt.Sprintf("Nothing to update %+v", v.instance.Status))

	return nil
}

func (v *VRGInstance) updateStatusState() {
	dataReadyCondition := findCondition(v.instance.Status.Conditions, VRGConditionTypeDataReady)
	if dataReadyCondition == nil {
		v.log.Info("Failed to find the DataReady condition in status")

		return
	}

	StatusState := getStatusStateFromSpecState(v.instance.Spec.ReplicationState)

	// update Status.State to reflect the state in spec
	// only after successful transition of the resource
	// (from primary->secondary or vise versa). That
	// successful completion of transition can be seen
	// in dataReadyCondition.Status being set to True.
	if dataReadyCondition.Status == metav1.ConditionTrue {
		v.instance.Status.State = StatusState

		return
	}

	// If VRG available condition is not true and the reason
	// is Error, then mark Status.State as UnknownState instead
	// of Primary or Secondary.
	if dataReadyCondition.Reason == VRGConditionReasonError {
		v.instance.Status.State = ramendrv1alpha1.UnknownState

		return
	}

	// If the state in spec is anything apart from
	// primary or secondary, then explicitly set
	// the Status.State to UnknownState.
	if StatusState == ramendrv1alpha1.UnknownState {
		v.instance.Status.State = StatusState
	}
}

func getStatusStateFromSpecState(state ramendrv1alpha1.ReplicationState) ramendrv1alpha1.State {
	switch state {
	case ramendrv1alpha1.Primary:
		return ramendrv1alpha1.PrimaryState
	case ramendrv1alpha1.Secondary:
		return ramendrv1alpha1.SecondaryState
	default:
		return ramendrv1alpha1.UnknownState
	}
}

// updateVRGConditions updates three summary conditions VRGConditionTypeDataReady,
// VRGConditionTypeClusterDataProtected and VRGConditionDataProtected at the VRG
// level based on the corresponding PVC level conditions in the VRG:
//
// The VRGConditionTypeClusterDataReady summary condition is not a PVC level
// condition and is updated elsewhere.
func (v *VRGInstance) updateVRGConditions() {
	if (len(v.volRepPVCs) == 0) && (len(v.volSyncPVCs) > 0) &&
		(v.instance.Spec.ReplicationState == ramendrv1alpha1.Primary) {
		volSyncSrcSetup := findCondition(v.instance.Status.Conditions, VRGConditionTypeVolSyncRepSourceSetup)
		if volSyncSrcSetup != nil && volSyncSrcSetup.Status == metav1.ConditionTrue {
			setVRGAsPrimaryReadyCondition(&v.instance.Status.Conditions, v.instance.Generation,
				"All PVCs in the VolumeReplicationGroup are VolSync ready")
			setVRGAsDataProtectedCondition(&v.instance.Status.Conditions, v.instance.Generation,
				"All PVCs in the VolumeReplicationGroup are VolSync data protected")
			setVRGClusterDataProtectedCondition(&v.instance.Status.Conditions, v.instance.Generation,
				"All Cluster data of all PVs are protected")

			return
		}
	}

	v.updateVRGDataReadyCondition()
	v.updateVRGDataProtectedCondition()
	v.updateVRGClusterDataProtectedCondition()
}

//
// Follow this logic to update VRG (and also ProtectedPVC) conditions for VolRep
// while reconciling VolumeReplicationGroup resource.
//
// For both Primary and Secondary:
// if getting VolRep fails and volrep does not exist:
//    ProtectedPVC.conditions.Available.Status = False
//    ProtectedPVC.conditions.Available.Reason = Progressing
//    return
// if getting VolRep fails and some other error:
//    ProtectedPVC.conditions.Available.Status = Unknown
//    ProtectedPVC.conditions.Available.Reason = Error
//
// This below if condition check helps in undersanding whether
// promotion/demotion has been successfully completed or not.
// if VolRep.Status.Conditions[Completed].Status == True
//    ProtectedPVC.conditions.Available.Status = True
//    ProtectedPVC.conditions.Available.Reason = Replicating
// else
//    ProtectedPVC.conditions.Available.Status = False
//    ProtectedPVC.conditions.Available.Reason = Error
//
// if all ProtectedPVCs are Replicating, then
//    VRG.conditions.Available.Status = true
//    VRG.conditions.Available.Reason = Replicating
// if atleast one ProtectedPVC.conditions[Available].Reason == Error
//    VRG.conditions.Available.Status = false
//    VRG.conditions.Available.Reason = Error
// if no ProtectedPVCs is in error and atleast one is progressing, then
//    VRG.conditions.Available.Status = false
//    VRG.conditions.Available.Reason = Progressing
//
func (v *VRGInstance) updateVRGDataReadyCondition() {
	if (len(v.volRepPVCs) == 0) && (len(v.volSyncPVCs) > 0) {
		v.vrgReadyStatus()

		return
	}

	vrgReady := len(v.instance.Status.ProtectedPVCs) != 0
	vrgProgressing := false

	for _, protectedPVC := range v.instance.Status.ProtectedPVCs {
		dataReadyCondition := findCondition(protectedPVC.Conditions, VRGConditionTypeDataReady)
		if dataReadyCondition == nil {
			vrgReady = false
			// When will we hit this condition? If it is due to a race condition,
			// why treat it as an error instead of progressing?
			v.log.Info(fmt.Sprintf("Failed to find condition %s for vrg %s/%s", VRGConditionTypeDataReady,
				v.instance.Name, v.instance.Namespace))

			break
		}

		if dataReadyCondition.Reason == VRGConditionReasonProgressing {
			vrgReady = false
			vrgProgressing = true
			// Breaking out in this case may be incorrect, as another PVC could
			// have a more serious `error` condition, isn't it?
			break
		}

		if dataReadyCondition.Reason == VRGConditionReasonError ||
			dataReadyCondition.Reason == VRGConditionReasonErrorUnknown {
			vrgReady = false
			// If there is even a single protected pvc that saw an error,
			// then entire VRG should mark its condition as error. Set
			// vrgPogressing to false.
			vrgProgressing = false

			v.log.Info(fmt.Sprintf("Condition %s has error reason %s for vrg %s/%s", VRGConditionTypeDataReady,
				dataReadyCondition.Reason, v.instance.Name, v.instance.Namespace))

			break
		}
	}

	if vrgReady {
		v.vrgReadyStatus()

		return
	}

	if vrgProgressing {
		v.log.Info("Marking VRG not DataReady with progressing reason")

		msg := "VolumeReplicationGroup is progressing"
		setVRGDataProgressingCondition(&v.instance.Status.Conditions, v.instance.Generation, msg)

		return
	}

	// None of the VRG Ready and VRG Progressing conditions are met.
	// Set Error condition for VRG.
	v.log.Info("Marking VRG not DataReady with error. All PVCs are not ready")

	msg := "All PVCs of the VolumeReplicationGroup are not ready"
	setVRGDataErrorCondition(&v.instance.Status.Conditions, v.instance.Generation, msg)
}

func (v *VRGInstance) updateVRGDataProtectedCondition() {
	if (len(v.volRepPVCs) == 0) && (len(v.volSyncPVCs) > 0) {
		v.log.Info("Marking VRG data protected after completing replication")

		msg := "PVCs in the VolumeReplicationGroup are data protected "
		setVRGAsDataProtectedCondition(&v.instance.Status.Conditions, v.instance.Generation, msg)

		return
	}

	vrgProtected := true
	vrgReplicating := false

	for _, protectedPVC := range v.instance.Status.ProtectedPVCs {
		dataProtectedCondition := findCondition(protectedPVC.Conditions, VRGConditionTypeDataProtected)
		if dataProtectedCondition == nil {
			vrgProtected = false
			vrgReplicating = false

			v.log.Info(fmt.Sprintf("Failed to find condition %s for vrg", VRGConditionTypeDataProtected))

			break
		}

		// VRGConditionReasonReplicating => VRG secondary, VRGConditionReasonReady => VRG Primary
		if dataProtectedCondition.Reason == VRGConditionReasonReplicating ||
			dataProtectedCondition.Reason == VRGConditionReasonReady {
			vrgProtected = false
			vrgReplicating = true

			continue
		}

		if dataProtectedCondition.Reason == VRGConditionReasonError ||
			dataProtectedCondition.Reason == VRGConditionReasonErrorUnknown {
			vrgProtected = false
			// Even a single pvc seeing error means, entire VRG marks this
			// condition as error. Set vrgReplicating to false
			vrgReplicating = false

			v.log.Info(fmt.Sprintf("Condition %s has error reason %s for vrg",
				VRGConditionTypeDataProtected, dataProtectedCondition.Reason))

			break
		}
	}

	if vrgProtected {
		v.log.Info("Marking VRG data protected after completing replication")

		msg := "PVCs in the VolumeReplicationGroup are data protected "
		setVRGAsDataProtectedCondition(&v.instance.Status.Conditions, v.instance.Generation, msg)

		return
	}

	if vrgReplicating {
		v.log.Info("Marking VRG data protection false with replicating reason")

		msg := "VolumeReplicationGroup is replicating"
		setVRGDataProtectionProgressCondition(&v.instance.Status.Conditions, v.instance.Generation, msg)

		return
	}

	// VRG is neither Data Protected nor Replicating
	v.log.Info("Marking VRG data not protected with error. All PVCs are not ready")

	msg := "All PVCs of the VolumeReplicationGroup are not ready"
	setVRGAsDataNotProtectedCondition(&v.instance.Status.Conditions, v.instance.Generation, msg)
}

func (v *VRGInstance) vrgReadyStatus() {
	if v.instance.Spec.ReplicationState == ramendrv1alpha1.Secondary {
		v.log.Info("Marking VRG ready with replicating reason")

		msg := "PVCs in the VolumeReplicationGroup group are replicating"
		setVRGDataReplicatingCondition(&v.instance.Status.Conditions, v.instance.Generation, msg)

		return
	}

	// VRG as primary
	v.log.Info("Marking VRG data ready after establishing replication")

	msg := "PVCs in the VolumeReplicationGroup are ready for use"
	setVRGAsPrimaryReadyCondition(&v.instance.Status.Conditions, v.instance.Generation, msg)
}

// updateVRGClusterDataProtectedCondition updates the VRG summary level
// cluster data protected condition based on individual PVC's cluster data
// protected condition.  If at least one PVC is experiencing an error condition,
// set the VRG level condition to error.  If not, if at least one PVC is in a
// protecting condition, set the VRG level condition to protecting.  If not, set
// the VRG level condition to true.
func (v *VRGInstance) updateVRGClusterDataProtectedCondition() {
	if (len(v.volRepPVCs) == 0) && (len(v.volSyncPVCs) > 0) {
		v.log.Info("Marking VRG data protected")

		msg := "Cluster data of all PVs are protected"
		setVRGClusterDataProtectedCondition(&v.instance.Status.Conditions, v.instance.Generation, msg)

		return
	}

	atleastOneProtecting := false
	atleastOneError := false

	for _, protectedPVC := range v.instance.Status.ProtectedPVCs {
		clusterDataProtectedCondition := findCondition(protectedPVC.Conditions,
			VRGConditionTypeClusterDataProtected)
		if clusterDataProtectedCondition == nil ||
			clusterDataProtectedCondition.Reason == VRGConditionReasonUploading {
			atleastOneProtecting = true
			// Continue to check if there are other PVCs that have an error
			// condition.
			continue
		}

		if clusterDataProtectedCondition.Reason != VRGConditionReasonUploaded {
			atleastOneError = true
			// A single PVC with an error condition is sufficient to affect the
			// entire VRG; no need to check other PVCs.
			break
		}
	}

	if atleastOneError {
		msg := "Cluster data of one or more PVs are unprotected"
		setVRGClusterDataUnprotectedCondition(&v.instance.Status.Conditions, v.instance.Generation, msg)
		v.log.Info(msg)

		return
	}

	if atleastOneProtecting {
		msg := "Cluster data of one or more PVs are in the process of being protected"
		setVRGClusterDataProtectingCondition(&v.instance.Status.Conditions, v.instance.Generation, msg)
		v.log.Info(msg)

		return
	}

	// All PVCs in the VRG are in protected state because not a single PVC is in
	// error condition and not a single PVC is in protecting condition.  Hence,
	// the VRG's cluster data protection condition is met.
	msg := "Cluster data of all PVs are protected"
	setVRGClusterDataProtectedCondition(&v.instance.Status.Conditions, v.instance.Generation, msg)
	v.log.Info(msg)
}

// It might be better move the helper functions like these to a separate
// package or a separate go file?
func containsString(values []string, s string) bool {
	for _, item := range values {
		if item == s {
			return true
		}
	}

	return false
}

func removeString(values []string, s string) []string {
	result := []string{}

	for _, item := range values {
		if item == s {
			continue
		}

		result = append(result, item)
	}

	return result
}