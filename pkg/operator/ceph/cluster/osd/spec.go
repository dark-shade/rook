/*
Copyright 2016 The Rook Authors. All rights reserved.

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

// Package osd for the Ceph OSDs.
package osd

import (
	"encoding/json"
	"fmt"
	"path"
	"strconv"
	"strings"

	"github.com/pkg/errors"
	cephv1 "github.com/rook/rook/pkg/apis/ceph.rook.io/v1"
	opmon "github.com/rook/rook/pkg/operator/ceph/cluster/mon"
	"github.com/rook/rook/pkg/operator/ceph/cluster/osd/config"
	opconfig "github.com/rook/rook/pkg/operator/ceph/config"
	"github.com/rook/rook/pkg/operator/ceph/controller"
	"github.com/rook/rook/pkg/operator/k8sutil"
	apps "k8s.io/api/apps/v1"
	batch "k8s.io/api/batch/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	osdStoreEnvVarName                  = "ROOK_OSD_STORE"
	osdDatabaseSizeEnvVarName           = "ROOK_OSD_DATABASE_SIZE"
	osdWalSizeEnvVarName                = "ROOK_OSD_WAL_SIZE"
	osdJournalSizeEnvVarName            = "ROOK_OSD_JOURNAL_SIZE"
	osdsPerDeviceEnvVarName             = "ROOK_OSDS_PER_DEVICE"
	encryptedDeviceEnvVarName           = "ROOK_ENCRYPTED_DEVICE"
	osdMetadataDeviceEnvVarName         = "ROOK_METADATA_DEVICE"
	pvcBackedOSDVarName                 = "ROOK_PVC_BACKED_OSD"
	blockPathVarName                    = "ROOK_BLOCK_PATH"
	cvModeVarName                       = "ROOK_CV_MODE"
	lvBackedPVVarName                   = "ROOK_LV_BACKED_PV"
	CrushDeviceClassVarName             = "ROOK_OSD_CRUSH_DEVICE_CLASS"
	rookBinariesMountPath               = "/rook"
	rookBinariesVolumeName              = "rook-binaries"
	activateOSDVolumeName               = "activate-osd"
	activateOSDMountPath                = "/var/lib/ceph/osd/ceph-"
	blockPVCMapperInitContainer         = "blkdevmapper"
	blockPVCMetadataMapperInitContainer = "blkdevmapper-metadata"
	activatePVCOSDInitContainer         = "activate"
	expandPVCOSDInitContainer           = "expand-bluefs"
	// CephDeviceSetLabelKey is the Rook device set label key
	CephDeviceSetLabelKey = "ceph.rook.io/DeviceSet"
	// CephSetIndexLabelKey is the Rook label key index
	CephSetIndexLabelKey = "ceph.rook.io/setIndex"
	// CephDeviceSetPVCIDLabelKey is the Rook PVC ID label key
	CephDeviceSetPVCIDLabelKey = "ceph.rook.io/DeviceSetPVCId"
	// OSDOverPVCLabelKey is the Rook PVC label key
	OSDOverPVCLabelKey = "ceph.rook.io/pvc"
	udevPath           = "/run/udev"
	udevVolName        = "run-udev"
)

const (
	activateOSDCode = `
set -ex

OSD_ID=%s
OSD_UUID=%s
OSD_STORE_FLAG="%s"
OSD_DATA_DIR=/var/lib/ceph/osd/ceph-"$OSD_ID"
CV_MODE=%s
DEVICE=%s
METADATA_DEVICE="$%s"

# active the osd with ceph-volume
if [[ "$CV_MODE" == "lvm" ]]; then
	TMP_DIR=$(mktemp -d)

	# activate osd
	ceph-volume "$CV_MODE" activate --no-systemd "$OSD_STORE_FLAG" "$OSD_ID" "$OSD_UUID"

	# copy the tmpfs directory to a temporary directory
	# this is needed because when the init container exits, the tmpfs goes away and its content with it
	# this will result in the emptydir to be empty when accessed by the main osd container
	cp --verbose --no-dereference "$OSD_DATA_DIR"/* "$TMP_DIR"/

	# unmount the tmpfs since we don't need it anymore
	umount "$OSD_DATA_DIR"

	# copy back the content of the tmpfs into the original osd directory
	cp --verbose --no-dereference "$TMP_DIR"/* "$OSD_DATA_DIR"

	# retain ownership of files to the ceph user/group
	chown --verbose --recursive ceph:ceph "$OSD_DATA_DIR"

	# remove the temporary directory
	rm --recursive --force "$TMP_DIR"
else
	ARGS=(--device ${DEVICE} --no-systemd --no-tmpfs)
	if [ -n "$METADATA_DEVICE" ]; then
		ARGS+=(--block.db ${METADATA_DEVICE})
	fi
	# ceph-volume raw mode only supports bluestore so we don't need to pass a store flag
	ceph-volume "$CV_MODE" activate "${ARGS[@]}"
fi

`
)

// OSDs on PVC using a certain storage class need to do some tuning
const (
	osdRecoverySleep = "0.1"
	osdSnapTrimSleep = "2"
	osdDeleteSleep   = "2"
)

func (c *Cluster) makeJob(osdProps osdProperties, provisionConfig *provisionConfig) (*batch.Job, error) {
	podSpec, err := c.provisionPodTemplateSpec(osdProps, v1.RestartPolicyOnFailure, provisionConfig)
	if err != nil {
		return nil, err
	}

	if !osdProps.onPVC() {
		podSpec.Spec.NodeSelector = map[string]string{v1.LabelHostname: osdProps.crushHostname}
	} else {
		// This is not needed in raw mode and 14.2.8 brings it
		// but we still want to do this not to lose backward compatibility with lvm based OSDs...
		podSpec.Spec.InitContainers = append(podSpec.Spec.InitContainers, c.getPVCInitContainer(osdProps))
		if osdProps.onPVCWithMetadata() {
			podSpec.Spec.InitContainers = append(podSpec.Spec.InitContainers, c.getPVCMetadataInitContainer("/srv", osdProps))
		}
	}

	job := &batch.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      k8sutil.TruncateNodeName(prepareAppNameFmt, osdProps.crushHostname),
			Namespace: c.clusterInfo.Namespace,
			Labels: map[string]string{
				k8sutil.AppAttr:     prepareAppName,
				k8sutil.ClusterAttr: c.clusterInfo.Namespace,
			},
		},
		Spec: batch.JobSpec{
			Template: *podSpec,
		},
	}

	if osdProps.onPVC() {
		k8sutil.AddLabelToJob(OSDOverPVCLabelKey, osdProps.pvc.ClaimName, job)
	}

	k8sutil.AddRookVersionLabelToJob(job)
	controller.AddCephVersionLabelToJob(c.clusterInfo.CephVersion, job)
	k8sutil.SetOwnerRef(&job.ObjectMeta, &c.clusterInfo.OwnerRef)
	return job, nil
}

func (c *Cluster) makeDeployment(osdProps osdProperties, osd OSDInfo, provisionConfig *provisionConfig) (*apps.Deployment, error) {
	// If running on Octopus, we don't need to use the host PID namespace
	var hostPID = !c.clusterInfo.CephVersion.IsAtLeastOctopus()
	deploymentName := fmt.Sprintf(osdAppNameFmt, osd.ID)
	replicaCount := int32(1)
	volumeMounts := controller.CephVolumeMounts(provisionConfig.DataPathMap, false)
	configVolumeMounts := controller.RookVolumeMounts(provisionConfig.DataPathMap, false)
	// When running on PVC, the OSDs don't need a bindmount on dataDirHostPath, only the monitors do
	if osdProps.onPVC() {
		c.spec.DataDirHostPath = ""
	}
	volumes := controller.PodVolumes(provisionConfig.DataPathMap, c.spec.DataDirHostPath, false)
	failureDomainValue := osdProps.crushHostname
	doConfigInit := true       // initialize ceph.conf in init container?
	doBinaryCopyInit := true   // copy tini and rook binaries in an init container?
	doActivateOSDInit := false // run an init container to activate the osd?

	// If CVMode is empty, this likely means we upgraded Rook
	// This property did not exist before so we need to initialize it
	// This property is used for both PVC and non-PVC use case
	if osd.CVMode == "" {
		osd.CVMode = "lvm"
	}

	dataDir := k8sutil.DataDir
	// Create volume config for /dev so the pod can access devices on the host
	// Only valid when running OSD with LVM mode
	if osd.CVMode == "lvm" {
		devVolume := v1.Volume{Name: "devices", VolumeSource: v1.VolumeSource{HostPath: &v1.HostPathVolumeSource{Path: "/dev"}}}
		volumes = append(volumes, devVolume)
		devMount := v1.VolumeMount{Name: "devices", MountPath: "/dev"}
		volumeMounts = append(volumeMounts, devMount)
	}

	// If the OSD runs on PVC
	if osdProps.onPVC() {
		// Create volume config for PVCs
		volumes = append(volumes, getPVCOSDVolumes(&osdProps)...)
	}

	if len(volumes) == 0 {
		return nil, errors.New("empty volumes")
	}

	storeType := config.Bluestore
	osdID := strconv.Itoa(osd.ID)
	tiniEnvVar := v1.EnvVar{Name: "TINI_SUBREAPER", Value: ""}
	envVars := append(c.getConfigEnvVars(osdProps, dataDir), []v1.EnvVar{
		tiniEnvVar,
	}...)
	envVars = append(envVars, k8sutil.ClusterDaemonEnvVars(c.spec.CephVersion.Image)...)
	envVars = append(envVars, []v1.EnvVar{
		{Name: "ROOK_OSD_UUID", Value: osd.UUID},
		{Name: "ROOK_OSD_ID", Value: osdID},
		{Name: "ROOK_OSD_STORE_TYPE", Value: storeType},
		{Name: "ROOK_CEPH_MON_HOST",
			ValueFrom: &v1.EnvVarSource{
				SecretKeyRef: &v1.SecretKeySelector{LocalObjectReference: v1.LocalObjectReference{
					Name: "rook-ceph-config"},
					Key: "mon_host"}}},
		{Name: "CEPH_ARGS", Value: "-m $(ROOK_CEPH_MON_HOST)"},
	}...)
	configEnvVars := append(c.getConfigEnvVars(osdProps, dataDir), []v1.EnvVar{
		tiniEnvVar,
		{Name: "ROOK_OSD_ID", Value: osdID},
		{Name: "ROOK_CEPH_VERSION", Value: c.clusterInfo.CephVersion.CephVersionFormatted()},
		{Name: "ROOK_IS_DEVICE", Value: "true"},
	}...)

	// If the OSD runs on PVC
	if osdProps.onPVC() {
		// add the PVC size to the pod spec so that if the size changes the OSD will be restarted and pick up the change
		envVars = append(envVars, v1.EnvVar{Name: "ROOK_OSD_PVC_SIZE", Value: osdProps.pvcSize})

		// Append tuning flag if necessary
		if osdProps.tuneSlowDeviceClass {
			err := c.osdRunFlagTuningOnPVC(osd.ID)
			if err != nil {
				return nil, errors.Wrapf(err, "failed to apply tuning on osd %q", strconv.Itoa(osd.ID))
			}
		}
	}

	var command []string
	var args []string
	// If the OSD was prepared with ceph-volume and running on PVC and using the LVM mode
	if osdProps.onPVC() && osd.CVMode == "lvm" {
		// if the osd was provisioned by ceph-volume, we need to launch it with rook as the parent process
		command = []string{path.Join(rookBinariesMountPath, "tini")}
		args = []string{
			"--", path.Join(rookBinariesMountPath, "rook"),
			"ceph", "osd", "start",
			"--",
			"--foreground",
			"--id", osdID,
			"--fsid", c.clusterInfo.FSID,
			"--cluster", "ceph",
			"--setuser", "ceph",
			"--setgroup", "ceph",
			fmt.Sprintf("--crush-location=%s", osd.Location),
		}
	} else if osdProps.onPVC() && osd.CVMode == "raw" {
		doBinaryCopyInit = false
		doConfigInit = false
		command = []string{"ceph-osd"}
		args = []string{
			"--foreground",
			"--id", osdID,
			"--fsid", c.clusterInfo.FSID,
			"--setuser", "ceph",
			"--setgroup", "ceph",
			fmt.Sprintf("--crush-location=%s", osd.Location),
		}
	} else {
		doBinaryCopyInit = false
		doConfigInit = false
		doActivateOSDInit = true
		command = []string{"ceph-osd"}
		args = []string{
			"--foreground",
			"--id", osdID,
			"--fsid", c.clusterInfo.FSID,
			"--setuser", "ceph",
			"--setgroup", "ceph",
			fmt.Sprintf("--crush-location=%s", osd.Location),
		}
	}

	// The osd itself needs to talk to udev to report information about the device (vendor/serial etc)
	udevVolume, udevVolumeMount := getUdevVolume()
	volumes = append(volumes, udevVolume)
	volumeMounts = append(volumeMounts, udevVolumeMount)

	// Add the volume to the spec and the mount to the daemon container
	copyBinariesVolume, copyBinariesContainer := c.getCopyBinariesContainer()
	if doBinaryCopyInit {
		volumes = append(volumes, copyBinariesVolume)
		volumeMounts = append(volumeMounts, copyBinariesContainer.VolumeMounts[0])
	}

	// Add the volume to the spec and the mount to the daemon container
	// so that it can pick the already mounted/activated osd metadata path
	// This container will activate the OSD and place the activated filesinto an empty dir
	// The empty dir will be shared by the "activate-osd" pod and the "osd" main pod
	activateOSDVolume, activateOSDContainer := c.getActivateOSDInitContainer(osdID, osd, osdProps)
	if doActivateOSDInit {
		volumes = append(volumes, activateOSDVolume)
		volumeMounts = append(volumeMounts, activateOSDContainer.VolumeMounts[0])
	}

	args = append(args, opconfig.LoggingFlags()...)
	args = append(args, osdOnSDNFlag(c.spec.Network)...)

	osdDataDirPath := activateOSDMountPath + osdID
	if osdProps.onPVC() && osd.CVMode == "lvm" {
		// Let's use the old bridge for these lvm based pvc osds
		volumeMounts = append(volumeMounts, getPvcOSDBridgeMount(osdProps.pvc.ClaimName))
		envVars = append(envVars, pvcBackedOSDEnvVar("true"))
		envVars = append(envVars, blockPathEnvVariable(osd.BlockPath))
		envVars = append(envVars, cvModeEnvVariable(osd.CVMode))
		envVars = append(envVars, lvBackedPVEnvVar(strconv.FormatBool(osd.LVBackedPV)))
	}

	if osdProps.onPVC() && osd.CVMode == "raw" {
		volumeMounts = append(volumeMounts, getPvcOSDBridgeMountActivate(osdDataDirPath, osdProps.pvc.ClaimName))
		envVars = append(envVars, pvcBackedOSDEnvVar("true"))
		envVars = append(envVars, blockPathEnvVariable(osd.BlockPath))
		envVars = append(envVars, cvModeEnvVariable(osd.CVMode))
	}

	// We cannot go un-privileged until we have a bindmount for logs and crash
	// OpenShift requires privileged containers for that
	// If we remove those OSD on PVC with raw mode won't need to be privileged
	// We could try to run as ceph too, more investigations needed
	privileged := true
	runAsUser := int64(0)
	readOnlyRootFilesystem := false
	securityContext := &v1.SecurityContext{
		Privileged:             &privileged,
		RunAsUser:              &runAsUser,
		ReadOnlyRootFilesystem: &readOnlyRootFilesystem,
	}

	// needed for luksOpen synchronization when devices are encrypted and the osd is prepared with LVM
	hostIPC := osdProps.storeConfig.EncryptedDevice

	initContainers := make([]v1.Container, 0, 4)
	if doConfigInit {
		initContainers = append(initContainers,
			v1.Container{
				Args:            []string{"ceph", "osd", "init"},
				Name:            controller.ConfigInitContainerName,
				Image:           k8sutil.MakeRookImage(c.rookVersion),
				VolumeMounts:    configVolumeMounts,
				Env:             configEnvVars,
				SecurityContext: securityContext,
			})
	}
	if doBinaryCopyInit {
		initContainers = append(initContainers, *copyBinariesContainer)
	}
	if osdProps.onPVC() && osd.CVMode == "lvm" {
		initContainers = append(initContainers, c.getPVCInitContainer(osdProps))
	}

	if osdProps.onPVC() && osd.CVMode == "raw" {
		initContainers = append(initContainers, c.getPVCInitContainerActivate(osdDataDirPath, osdProps))
		if osdProps.onPVCWithMetadata() {
			initContainers = append(initContainers, c.getPVCMetadataInitContainerActivate(osdDataDirPath, osdProps))
		}
		initContainers = append(initContainers, c.getActivatePVCInitContainer(osdProps, osdID))
		initContainers = append(initContainers, c.getExpandPVCInitContainer(osdProps, osdID))
	}
	if doActivateOSDInit {
		initContainers = append(initContainers, *activateOSDContainer)
	}

	// For OSD on PVC with LVM the directory does not exist yet
	// It gets created by the 'ceph-volume lvm activate' command
	//
	// 	So OSD non-PVC the directory has been created by the 'activate' container already and has chown it
	// So we don't need to chown it again
	dataPath := ""

	// Raw mode on PVC needs this path so that OSD's metadata files can be chown after 'ceph-bluestore-tool' ran
	if osd.CVMode == "raw" && osdProps.onPVC() {
		dataPath = activateOSDMountPath + osdID
	}

	// Doing a chown in a post start lifecycle hook does not reliably complete before the OSD
	// process starts, which can cause the pod to fail without the lifecycle hook's chown command
	// completing. It can take an arbitrarily long time for a pod restart to successfully chown the
	// directory. This is a race condition for all OSDs; therefore, do this in an init container.
	// See more discussion here: https://github.com/rook/rook/pull/3594#discussion_r312279176
	initContainers = append(initContainers,
		controller.ChownCephDataDirsInitContainer(
			opconfig.DataPathMap{ContainerDataDir: dataPath},
			c.spec.CephVersion.Image,
			volumeMounts,
			osdProps.resources,
			securityContext,
		))

	podTemplateSpec := v1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Name:   AppName,
			Labels: c.getOSDLabels(osd.ID, failureDomainValue, osdProps.portable),
		},
		Spec: v1.PodSpec{
			RestartPolicy:      v1.RestartPolicyAlways,
			ServiceAccountName: serviceAccountName,
			HostNetwork:        c.spec.Network.IsHost(),
			HostPID:            hostPID,
			HostIPC:            hostIPC,
			PriorityClassName:  cephv1.GetOSDPriorityClassName(c.spec.PriorityClassNames),
			InitContainers:     initContainers,
			Containers: []v1.Container{
				{
					Command:         command,
					Args:            args,
					Name:            "osd",
					Image:           c.spec.CephVersion.Image,
					VolumeMounts:    volumeMounts,
					Env:             envVars,
					Resources:       osdProps.resources,
					SecurityContext: securityContext,
					LivenessProbe:   controller.GenerateLivenessProbeExecDaemon(opconfig.OsdType, osdID),
				},
			},
			Volumes:       volumes,
			SchedulerName: osdProps.schedulerName,
		},
	}

	// If the liveness probe is enabled
	podTemplateSpec.Spec.Containers[0] = opconfig.ConfigureLivenessProbe(cephv1.KeyOSD, podTemplateSpec.Spec.Containers[0], c.spec.HealthCheck)

	if c.spec.Network.IsHost() {
		podTemplateSpec.Spec.DNSPolicy = v1.DNSClusterFirstWithHostNet
	} else if c.spec.Network.NetworkSpec.IsMultus() {
		k8sutil.ApplyMultus(c.spec.Network.NetworkSpec, &podTemplateSpec.ObjectMeta)
	}

	deployment := &apps.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      deploymentName,
			Namespace: c.clusterInfo.Namespace,
			Labels:    c.getOSDLabels(osd.ID, failureDomainValue, osdProps.portable),
		},
		Spec: apps.DeploymentSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					k8sutil.AppAttr:     AppName,
					k8sutil.ClusterAttr: c.clusterInfo.Namespace,
					OsdIdLabelKey:       fmt.Sprintf("%d", osd.ID),
				},
			},
			Strategy: apps.DeploymentStrategy{
				Type: apps.RecreateDeploymentStrategyType,
			},
			Template: podTemplateSpec,
			Replicas: &replicaCount,
		},
	}
	if osdProps.onPVC() {
		k8sutil.AddLabelToDeployment(OSDOverPVCLabelKey, osdProps.pvc.ClaimName, deployment)
		k8sutil.AddLabelToPod(OSDOverPVCLabelKey, osdProps.pvc.ClaimName, &deployment.Spec.Template)
	}
	if !osdProps.portable {
		deployment.Spec.Template.Spec.NodeSelector = map[string]string{v1.LabelHostname: osdProps.crushHostname}
	}
	// Replace default unreachable node toleration if the osd pod is portable and based in PVC
	if osdProps.onPVC() && osdProps.portable {
		k8sutil.AddUnreachableNodeToleration(&deployment.Spec.Template.Spec)
	}

	k8sutil.AddRookVersionLabelToDeployment(deployment)
	cephv1.GetOSDAnnotations(c.spec.Annotations).ApplyToObjectMeta(&deployment.ObjectMeta)
	cephv1.GetOSDAnnotations(c.spec.Annotations).ApplyToObjectMeta(&deployment.Spec.Template.ObjectMeta)
	controller.AddCephVersionLabelToDeployment(c.clusterInfo.CephVersion, deployment)
	controller.AddCephVersionLabelToDeployment(c.clusterInfo.CephVersion, deployment)
	k8sutil.SetOwnerRef(&deployment.ObjectMeta, &c.clusterInfo.OwnerRef)
	if !osdProps.onPVC() {
		cephv1.GetOSDPlacement(c.spec.Placement).ApplyToPodSpec(&deployment.Spec.Template.Spec)
	} else {
		osdProps.placement.ApplyToPodSpec(&deployment.Spec.Template.Spec)
	}

	return deployment, nil
}

// To get rook inside the container, the config init container needs to copy "tini" and "rook" binaries into a volume.
// Get the config flag so rook will copy the binaries and create the volume and mount that will be shared between
// the init container and the daemon container
func (c *Cluster) getCopyBinariesContainer() (v1.Volume, *v1.Container) {
	volume := v1.Volume{Name: rookBinariesVolumeName, VolumeSource: v1.VolumeSource{EmptyDir: &v1.EmptyDirVolumeSource{}}}
	mount := v1.VolumeMount{Name: rookBinariesVolumeName, MountPath: rookBinariesMountPath}

	return volume, &v1.Container{
		Args: []string{
			"copy-binaries",
			"--copy-to-dir", rookBinariesMountPath},
		Name:         "copy-bins",
		Image:        k8sutil.MakeRookImage(c.rookVersion),
		VolumeMounts: []v1.VolumeMount{mount},
	}
}

// This container runs all the actions needed to activate an OSD before we can run the OSD process
func (c *Cluster) getActivateOSDInitContainer(osdID string, osdInfo OSDInfo, osdProps osdProperties) (v1.Volume, *v1.Container) {
	volume := v1.Volume{Name: activateOSDVolumeName, VolumeSource: v1.VolumeSource{EmptyDir: &v1.EmptyDirVolumeSource{}}}
	envVars := osdActivateEnvVar()
	osdStore := "--bluestore"

	// Build empty dir osd path to something like "/var/lib/ceph/osd/ceph-0"
	activateOSDMountPathID := activateOSDMountPath + osdID

	volMounts := []v1.VolumeMount{
		{Name: activateOSDVolumeName, MountPath: activateOSDMountPathID},
		{Name: "devices", MountPath: "/dev"},
		{Name: k8sutil.ConfigOverrideName, ReadOnly: true, MountPath: opconfig.EtcCephDir},
	}

	if osdProps.onPVC() {
		volMounts = append(volMounts, getPvcOSDBridgeMount(osdProps.pvc.ClaimName))
	}

	container := &v1.Container{
		Command: []string{
			"/bin/bash",
			"-c",
			fmt.Sprintf(activateOSDCode, osdID, osdInfo.UUID, osdStore, osdInfo.CVMode, osdInfo.BlockPath, osdMetadataDeviceEnvVarName),
		},
		Name:            "activate",
		Image:           c.spec.CephVersion.Image,
		VolumeMounts:    volMounts,
		SecurityContext: PrivilegedContext(),
		Env:             envVars,
		Resources:       osdProps.resources,
	}

	return volume, container
}

func (c *Cluster) provisionPodTemplateSpec(osdProps osdProperties, restart v1.RestartPolicy, provisionConfig *provisionConfig) (*v1.PodTemplateSpec, error) {
	copyBinariesVolume, copyBinariesContainer := c.getCopyBinariesContainer()

	// ceph-volume is currently set up to use /etc/ceph/ceph.conf; this means no user config
	// overrides will apply to ceph-volume, but this is unnecessary anyway
	volumes := append(controller.PodVolumes(provisionConfig.DataPathMap, c.spec.DataDirHostPath, true), copyBinariesVolume)

	// create a volume on /dev so the pod can access devices on the host
	devVolume := v1.Volume{Name: "devices", VolumeSource: v1.VolumeSource{HostPath: &v1.HostPathVolumeSource{Path: "/dev"}}}
	volumes = append(volumes, devVolume)
	udevVolume := v1.Volume{Name: "udev", VolumeSource: v1.VolumeSource{HostPath: &v1.HostPathVolumeSource{Path: "/run/udev"}}}
	volumes = append(volumes, udevVolume)

	if osdProps.onPVC() {
		// Create volume config for PVCs
		volumes = append(volumes, getPVCOSDVolumes(&osdProps)...)
	}

	if len(volumes) == 0 {
		return nil, errors.New("empty volumes")
	}

	provisionContainer, err := c.provisionOSDContainer(osdProps, copyBinariesContainer.VolumeMounts[0], provisionConfig)
	if err != nil {
		return nil, errors.Wrap(err, "failed to generate OSD provisioning container")
	}

	podSpec := v1.PodSpec{
		ServiceAccountName: serviceAccountName,
		InitContainers: []v1.Container{
			*copyBinariesContainer,
		},
		Containers: []v1.Container{
			provisionContainer,
		},
		RestartPolicy:     restart,
		Volumes:           volumes,
		HostNetwork:       c.spec.Network.IsHost(),
		PriorityClassName: cephv1.GetOSDPriorityClassName(c.spec.PriorityClassNames),
		SchedulerName:     osdProps.schedulerName,
	}
	if c.spec.Network.IsHost() {
		podSpec.DNSPolicy = v1.DNSClusterFirstWithHostNet
	}
	if !osdProps.onPVC() {
		cephv1.GetOSDPlacement(c.spec.Placement).ApplyToPodSpec(&podSpec)
	} else {
		osdProps.placement.ApplyToPodSpec(&podSpec)
	}

	podMeta := metav1.ObjectMeta{
		Name: AppName,
		Labels: map[string]string{
			k8sutil.AppAttr:     prepareAppName,
			k8sutil.ClusterAttr: c.clusterInfo.Namespace,
			OSDOverPVCLabelKey:  osdProps.pvc.ClaimName,
		},
		Annotations: map[string]string{},
	}

	cephv1.GetOSDAnnotations(c.spec.Annotations).ApplyToObjectMeta(&podMeta)

	// ceph-volume --dmcrypt uses cryptsetup that synchronizes with udev on
	// host through semaphore
	podSpec.HostIPC = osdProps.storeConfig.EncryptedDevice

	return &v1.PodTemplateSpec{
		ObjectMeta: podMeta,
		Spec:       podSpec,
	}, nil
}

// Currently we can't mount a block mode pv directly to a privileged container
// So we mount it to a non privileged init container and then copy it to a common directory mounted inside init container
// and the privileged provision container.
func (c *Cluster) getPVCInitContainer(osdProps osdProperties) v1.Container {
	return v1.Container{
		Name:  blockPVCMapperInitContainer,
		Image: c.spec.CephVersion.Image,
		Command: []string{
			"cp",
		},
		Args: []string{"-a", fmt.Sprintf("/%s", osdProps.pvc.ClaimName), fmt.Sprintf("/mnt/%s", osdProps.pvc.ClaimName)},
		VolumeDevices: []v1.VolumeDevice{
			{
				Name:       osdProps.pvc.ClaimName,
				DevicePath: fmt.Sprintf("/%s", osdProps.pvc.ClaimName),
			},
		},
		VolumeMounts: []v1.VolumeMount{
			{
				MountPath: "/mnt",
				Name:      fmt.Sprintf("%s-bridge", osdProps.pvc.ClaimName),
			},
		},
		SecurityContext: opmon.PodSecurityContext(),
		Resources:       osdProps.resources,
	}
}

func (c *Cluster) getPVCInitContainerActivate(mountPath string, osdProps osdProperties) v1.Container {

	return v1.Container{
		Name:  blockPVCMapperInitContainer,
		Image: c.spec.CephVersion.Image,
		Command: []string{
			"cp",
		},
		Args: []string{"-a", fmt.Sprintf("/%s", osdProps.pvc.ClaimName), path.Join(mountPath, "block")},
		VolumeDevices: []v1.VolumeDevice{
			{
				Name:       osdProps.pvc.ClaimName,
				DevicePath: fmt.Sprintf("/%s", osdProps.pvc.ClaimName),
			},
		},
		VolumeMounts: []v1.VolumeMount{
			{
				MountPath: mountPath,
				SubPath:   path.Base(mountPath),
				Name:      fmt.Sprintf("%s-bridge", osdProps.pvc.ClaimName),
			},
		},
		SecurityContext: opmon.PodSecurityContext(),
		Resources:       osdProps.resources,
	}
}

// The reason why this is not part of getPVCInitContainer is that this will change the deployment spec object
// and thus restart the osd deployment, so it is better to have it separated and only enable it
// It will change the deployment spec because we must add a new argument to the method like 'mountPath' and use it in the container name
// otherwise we will end up with a new conflict during the job/deployment initialization
func (c *Cluster) getPVCMetadataInitContainer(mountPath string, osdProps osdProperties) v1.Container {
	return v1.Container{
		Name:  blockPVCMetadataMapperInitContainer,
		Image: c.spec.CephVersion.Image,
		Command: []string{
			"cp",
		},
		Args: []string{"-a", fmt.Sprintf("/%s", osdProps.metadataPVC.ClaimName), fmt.Sprintf("/srv/%s", osdProps.metadataPVC.ClaimName)},
		VolumeDevices: []v1.VolumeDevice{
			{
				Name:       osdProps.metadataPVC.ClaimName,
				DevicePath: fmt.Sprintf("/%s", osdProps.metadataPVC.ClaimName),
			},
		},
		VolumeMounts: []v1.VolumeMount{
			{
				MountPath: "/srv",
				Name:      fmt.Sprintf("%s-bridge", osdProps.metadataPVC.ClaimName),
			},
		},
		SecurityContext: opmon.PodSecurityContext(),
		Resources:       osdProps.resources,
	}
}

func (c *Cluster) getPVCMetadataInitContainerActivate(mountPath string, osdProps osdProperties) v1.Container {
	return v1.Container{
		Name:  blockPVCMetadataMapperInitContainer,
		Image: c.spec.CephVersion.Image,
		Command: []string{
			"cp",
		},
		Args: []string{"-a", fmt.Sprintf("/%s", osdProps.metadataPVC.ClaimName), path.Join(mountPath, "block.db")},
		VolumeDevices: []v1.VolumeDevice{
			{
				Name:       osdProps.metadataPVC.ClaimName,
				DevicePath: fmt.Sprintf("/%s", osdProps.metadataPVC.ClaimName),
			},
		},
		// We need to call getPvcOSDBridgeMountActivate() so that we can copy the metadata block into the "main" empty dir
		// This empty dir is passed along every init container
		VolumeMounts:    []v1.VolumeMount{getPvcOSDBridgeMountActivate(mountPath, osdProps.pvc.ClaimName)},
		SecurityContext: opmon.PodSecurityContext(),
		Resources:       osdProps.resources,
	}
}

func (c *Cluster) getActivatePVCInitContainer(osdProps osdProperties, osdID string) v1.Container {
	osdDataPath := activateOSDMountPath + osdID
	osdDataBlockPath := path.Join(osdDataPath, "block")

	container := v1.Container{
		Name:  activatePVCOSDInitContainer,
		Image: c.spec.CephVersion.Image,
		Command: []string{
			"ceph-bluestore-tool",
		},
		Args: []string{"prime-osd-dir", "--dev", osdDataBlockPath, "--path", osdDataPath, "--no-mon-config"},
		VolumeDevices: []v1.VolumeDevice{
			{
				Name:       osdProps.pvc.ClaimName,
				DevicePath: osdDataBlockPath,
			},
		},
		VolumeMounts:    []v1.VolumeMount{getPvcOSDBridgeMountActivate(osdDataPath, osdProps.pvc.ClaimName)},
		SecurityContext: PrivilegedContext(),
		Resources:       osdProps.resources,
	}

	return container
}

func (c *Cluster) getExpandPVCInitContainer(osdProps osdProperties, osdID string) v1.Container {
	osdDataPath := activateOSDMountPath + osdID

	container := v1.Container{
		Name:  expandPVCOSDInitContainer,
		Image: c.spec.CephVersion.Image,
		Command: []string{
			"ceph-bluestore-tool",
		},
		Args:            []string{"bluefs-bdev-expand", "--path", osdDataPath},
		VolumeMounts:    []v1.VolumeMount{getPvcOSDBridgeMountActivate(osdDataPath, osdProps.pvc.ClaimName)},
		SecurityContext: PrivilegedContext(),
		Resources:       osdProps.resources,
	}

	return container
}
func (c *Cluster) getConfigEnvVars(osdProps osdProperties, dataDir string) []v1.EnvVar {
	envVars := []v1.EnvVar{
		nodeNameEnvVar(osdProps.crushHostname),
		{Name: "ROOK_CLUSTER_ID", Value: string(c.clusterInfo.OwnerRef.UID)},
		k8sutil.PodIPEnvVar(k8sutil.PrivateIPEnvVar),
		k8sutil.PodIPEnvVar(k8sutil.PublicIPEnvVar),
		opmon.PodNamespaceEnvVar(c.clusterInfo.Namespace),
		opmon.EndpointEnvVar(),
		opmon.SecretEnvVar(),
		opmon.CephUsernameEnvVar(),
		opmon.CephSecretEnvVar(),
		k8sutil.ConfigDirEnvVar(dataDir),
		k8sutil.ConfigOverrideEnvVar(),
		{Name: "ROOK_FSID", ValueFrom: &v1.EnvVarSource{
			SecretKeyRef: &v1.SecretKeySelector{
				LocalObjectReference: v1.LocalObjectReference{Name: "rook-ceph-mon"},
				Key:                  "fsid",
			},
		}},
		k8sutil.NodeEnvVar(),
	}

	// Give a hint to the prepare pod for what the host in the CRUSH map should be
	crushmapHostname := osdProps.crushHostname
	if !osdProps.portable && osdProps.onPVC() {
		// If it's a pvc that's not portable we only know what the host name should be when inside the osd prepare pod
		crushmapHostname = ""
	}
	envVars = append(envVars, v1.EnvVar{Name: "ROOK_CRUSHMAP_HOSTNAME", Value: crushmapHostname})

	// Append ceph-volume environment variables
	envVars = append(envVars, cephVolumeEnvVar()...)

	if osdProps.storeConfig.DatabaseSizeMB != 0 {
		envVars = append(envVars, v1.EnvVar{Name: osdDatabaseSizeEnvVarName, Value: strconv.Itoa(osdProps.storeConfig.DatabaseSizeMB)})
	}

	if osdProps.storeConfig.WalSizeMB != 0 {
		envVars = append(envVars, v1.EnvVar{Name: osdWalSizeEnvVarName, Value: strconv.Itoa(osdProps.storeConfig.WalSizeMB)})
	}

	if osdProps.storeConfig.OSDsPerDevice != 0 {
		envVars = append(envVars, v1.EnvVar{Name: osdsPerDeviceEnvVarName, Value: strconv.Itoa(osdProps.storeConfig.OSDsPerDevice)})
	}

	if osdProps.storeConfig.EncryptedDevice {
		envVars = append(envVars, v1.EnvVar{Name: encryptedDeviceEnvVarName, Value: "true"})
	}

	return envVars
}

func (c *Cluster) provisionOSDContainer(osdProps osdProperties, copyBinariesMount v1.VolumeMount, provisionConfig *provisionConfig) (v1.Container, error) {
	envVars := c.getConfigEnvVars(osdProps, k8sutil.DataDir)

	// enable debug logging in the prepare job
	envVars = append(envVars, setDebugLogLevelEnvVar(true))

	// Drive Groups cannot be used to configure OSDs on PVCs, so ignore if this is a PVC config
	// This shouldn't ever happen, but do the PVC check to be sure
	if len(osdProps.driveGroups) > 0 && !osdProps.onPVC() {
		v, err := c.getDriveGroupEnvVar(osdProps)
		if err != nil {
			// Because OSD creation via drive groups should take precedent over other types of drive
			// creation, if there is an error here, we should fail. Allowing OSD creation to proceed
			// without drive group information could result in OSD configs being created which the
			// user does not want.
			return v1.Container{}, errors.Wrapf(err, "failed to get drive group info for OSD provisioning container")
		}
		// An env var with no name means there are no groups, don't add the var
		if v.Name != "" {
			envVars = append(envVars, v)
		}
	}

	// only 1 of device list, device filter, device path filter and use all devices can be specified.  We prioritize in that order.
	if len(osdProps.devices) > 0 {
		configuredDevices := []config.ConfiguredDevice{}
		for _, device := range osdProps.devices {
			id := device.Name
			if device.FullPath != "" {
				id = device.FullPath
			}
			cd := config.ConfiguredDevice{
				ID:          id,
				StoreConfig: config.ToStoreConfig(device.Config),
			}
			configuredDevices = append(configuredDevices, cd)
		}
		marshalledDevices, err := json.Marshal(configuredDevices)
		if err != nil {
			return v1.Container{}, errors.Wrapf(err, "failed to JSON marshal configured devices for node %q", osdProps.crushHostname)
		}
		envVars = append(envVars, dataDevicesEnvVar(string(marshalledDevices)))
	} else if osdProps.selection.DeviceFilter != "" {
		envVars = append(envVars, deviceFilterEnvVar(osdProps.selection.DeviceFilter))
	} else if osdProps.selection.DevicePathFilter != "" {
		envVars = append(envVars, devicePathFilterEnvVar(osdProps.selection.DevicePathFilter))
	} else if osdProps.selection.GetUseAllDevices() {
		envVars = append(envVars, deviceFilterEnvVar("all"))
	}
	envVars = append(envVars, v1.EnvVar{Name: "ROOK_CEPH_VERSION", Value: c.clusterInfo.CephVersion.CephVersionFormatted()})

	if osdProps.metadataDevice != "" {
		envVars = append(envVars, metadataDeviceEnvVar(osdProps.metadataDevice))
	}

	volumeMounts := append(controller.CephVolumeMounts(provisionConfig.DataPathMap, true), []v1.VolumeMount{
		{Name: "devices", MountPath: "/dev"},
		{Name: "udev", MountPath: "/run/udev"},
		copyBinariesMount,
	}...)

	// If the OSD runs on PVC
	if osdProps.onPVC() {
		volumeMounts = append(volumeMounts, getPvcOSDBridgeMount(osdProps.pvc.ClaimName))
		// The device list is read by the Rook CLI via environment variables so let's add them
		configuredDevices := []config.ConfiguredDevice{
			{
				ID:          fmt.Sprintf("/mnt/%s", osdProps.pvc.ClaimName),
				StoreConfig: config.NewStoreConfig(),
			},
		}
		if osdProps.onPVCWithMetadata() {
			volumeMounts = append(volumeMounts, getPvcMetadataOSDBridgeMount(osdProps.metadataPVC.ClaimName))
			configuredDevices = append(configuredDevices,
				config.ConfiguredDevice{
					ID:          fmt.Sprintf("/srv/%s", osdProps.metadataPVC.ClaimName),
					StoreConfig: config.NewStoreConfig(),
				})
		}
		marshalledDevices, err := json.Marshal(configuredDevices)
		if err != nil {
			return v1.Container{}, errors.Wrapf(err, "failed to JSON marshal configured devices for PVC %q", osdProps.crushHostname)
		}
		envVars = append(envVars, dataDevicesEnvVar(string(marshalledDevices)))
		envVars = append(envVars, pvcBackedOSDEnvVar("true"))
		envVars = append(envVars, crushDeviceClassEnvVar(osdProps.crushDeviceClass))
	}

	// run privileged always since we always mount /dev
	privileged := true
	runAsUser := int64(0)
	runAsNonRoot := false
	readOnlyRootFilesystem := false

	osdProvisionContainer := v1.Container{
		Command:      []string{path.Join(rookBinariesMountPath, "tini")},
		Args:         []string{"--", path.Join(rookBinariesMountPath, "rook"), "ceph", "osd", "provision"},
		Name:         "provision",
		Image:        c.spec.CephVersion.Image,
		VolumeMounts: volumeMounts,
		Env:          envVars,
		SecurityContext: &v1.SecurityContext{
			Privileged:             &privileged,
			RunAsUser:              &runAsUser,
			RunAsNonRoot:           &runAsNonRoot,
			ReadOnlyRootFilesystem: &readOnlyRootFilesystem,
		},
		Resources: cephv1.GetPrepareOSDResources(c.spec.Resources),
	}

	return osdProvisionContainer, nil
}

func getPvcOSDBridgeMount(claimName string) v1.VolumeMount {
	return v1.VolumeMount{Name: fmt.Sprintf("%s-bridge", claimName), MountPath: "/mnt"}
}

func getPvcOSDBridgeMountActivate(mountPath, claimName string) v1.VolumeMount {
	return v1.VolumeMount{Name: fmt.Sprintf("%s-bridge", claimName), MountPath: mountPath, SubPath: path.Base(mountPath)}
}

func getPvcMetadataOSDBridgeMount(claimName string) v1.VolumeMount {
	return v1.VolumeMount{Name: fmt.Sprintf("%s-bridge", claimName), MountPath: "/srv"}
}

func (c *Cluster) skipVolumeForDirectory(path string) bool {
	// If attempting to add a directory at /var/lib/rook, we need to skip the volume and volume mount
	// since the dataDirHostPath is always mounting at /var/lib/rook
	return path == k8sutil.DataDir
}

func getPVCOSDVolumes(osdProps *osdProperties) []v1.Volume {
	volumes := []v1.Volume{
		{
			Name: osdProps.pvc.ClaimName,
			VolumeSource: v1.VolumeSource{
				PersistentVolumeClaim: &osdProps.pvc,
			},
		},
		{
			// We need a bridge mount which is basically a common volume mount between the non privileged init container
			// and the privileged provision container or osd daemon container
			// The reason for this is mentioned in the comment for getPVCInitContainer() method
			Name: fmt.Sprintf("%s-bridge", osdProps.pvc.ClaimName),
			VolumeSource: v1.VolumeSource{
				EmptyDir: &v1.EmptyDirVolumeSource{
					Medium: "Memory",
				},
			},
		},
	}

	// If we have a metadata PVC let's add it
	if osdProps.onPVCWithMetadata() {
		metadataPVCVolume := []v1.Volume{
			{
				Name: osdProps.metadataPVC.ClaimName,
				VolumeSource: v1.VolumeSource{
					PersistentVolumeClaim: &osdProps.metadataPVC,
				},
			},
			{
				// We need a bridge mount which is basically a common volume mount between the non privileged init container
				// and the privileged provision container or osd daemon container
				// The reason for this is mentioned in the comment for getPVCInitContainer() method
				Name: fmt.Sprintf("%s-bridge", osdProps.metadataPVC.ClaimName),
				VolumeSource: v1.VolumeSource{
					EmptyDir: &v1.EmptyDirVolumeSource{
						Medium: "Memory",
					},
				},
			},
		}

		volumes = append(volumes, metadataPVCVolume...)
	}

	logger.Debugf("volumes are %+v", volumes)

	return volumes
}

func (c *Cluster) getDriveGroupEnvVar(osdProps osdProperties) (v1.EnvVar, error) {
	if len(osdProps.driveGroups) == 0 {
		return v1.EnvVar{}, nil
	}

	b, err := MarshalAsDriveGroupBlobs(osdProps.driveGroups)
	if err != nil {
		return v1.EnvVar{}, errors.Wrap(err, "failed to marshal drive groups into an env var")
	}
	return driveGroupsEnvVar(b), nil
}

func nodeNameEnvVar(name string) v1.EnvVar {
	return v1.EnvVar{Name: "ROOK_NODE_NAME", Value: name}
}

func driveGroupsEnvVar(driveGroups string) v1.EnvVar {
	return v1.EnvVar{Name: "ROOK_DRIVE_GROUPS", Value: driveGroups}
}

func dataDevicesEnvVar(dataDevices string) v1.EnvVar {
	return v1.EnvVar{Name: "ROOK_DATA_DEVICES", Value: dataDevices}
}

func deviceFilterEnvVar(filter string) v1.EnvVar {
	return v1.EnvVar{Name: "ROOK_DATA_DEVICE_FILTER", Value: filter}
}

func devicePathFilterEnvVar(filter string) v1.EnvVar {
	return v1.EnvVar{Name: "ROOK_DATA_DEVICE_PATH_FILTER", Value: filter}
}

func metadataDeviceEnvVar(metadataDevice string) v1.EnvVar {
	return v1.EnvVar{Name: osdMetadataDeviceEnvVarName, Value: metadataDevice}
}

func pvcBackedOSDEnvVar(pvcBacked string) v1.EnvVar {
	return v1.EnvVar{Name: pvcBackedOSDVarName, Value: pvcBacked}
}

func setDebugLogLevelEnvVar(debug bool) v1.EnvVar {
	level := "INFO"
	if debug {
		level = "DEBUG"
	}
	return v1.EnvVar{Name: "ROOK_LOG_LEVEL", Value: level}
}

func blockPathEnvVariable(lvPath string) v1.EnvVar {
	return v1.EnvVar{Name: blockPathVarName, Value: lvPath}
}

func cvModeEnvVariable(cvMode string) v1.EnvVar {
	return v1.EnvVar{Name: cvModeVarName, Value: cvMode}
}

func lvBackedPVEnvVar(lvBackedPV string) v1.EnvVar {
	return v1.EnvVar{Name: lvBackedPVVarName, Value: lvBackedPV}
}

func crushDeviceClassEnvVar(crushDeviceClass string) v1.EnvVar {
	return v1.EnvVar{Name: CrushDeviceClassVarName, Value: crushDeviceClass}
}

func osdOnSDNFlag(network cephv1.NetworkSpec) []string {
	var args []string
	// OSD fails to find the right IP to bind to when running on SDN
	// for more details: https://github.com/rook/rook/issues/3140
	if !network.IsHost() {
		args = append(args, "--ms-learn-addr-from-peer=false")
	}

	return args
}

func makeStorageClassDeviceSetPVCID(storageClassDeviceSetName string, setIndex int) (pvcID, pvcLabelSelector string) {
	pvcStorageClassDeviceSetPVCId := fmt.Sprintf("%s-%d", storageClassDeviceSetName, setIndex)
	return pvcStorageClassDeviceSetPVCId, fmt.Sprintf("%s=%s", CephDeviceSetPVCIDLabelKey, pvcStorageClassDeviceSetPVCId)
}

// This is the new function that generates the labels
// It includes the pvcTemplateName in it
func makeStorageClassDeviceSetPVCIDNew(storageClassDeviceSetName, pvcTemplateName string, setIndex int) (pvcID, pvcLabelSelector string) {
	pvcStorageClassDeviceSetPVCId := fmt.Sprintf("%s-%s-%d", storageClassDeviceSetName, strings.Replace(pvcTemplateName, " ", "-", -1), setIndex)
	return pvcStorageClassDeviceSetPVCId, fmt.Sprintf("%s=%s", CephDeviceSetPVCIDLabelKey, pvcStorageClassDeviceSetPVCId)
}

func makeStorageClassDeviceSetPVCLabel(storageClassDeviceSetName, pvcStorageClassDeviceSetPVCId string, setIndex int) map[string]string {
	return map[string]string{
		CephDeviceSetLabelKey:      storageClassDeviceSetName,
		CephSetIndexLabelKey:       fmt.Sprintf("%d", setIndex),
		CephDeviceSetPVCIDLabelKey: pvcStorageClassDeviceSetPVCId,
	}
}

func (c *Cluster) getOSDLabels(osdID int, failureDomainValue string, portable bool) map[string]string {
	return map[string]string{
		k8sutil.AppAttr:     AppName,
		k8sutil.ClusterAttr: c.clusterInfo.Namespace,
		OsdIdLabelKey:       fmt.Sprintf("%d", osdID),
		FailureDomainKey:    failureDomainValue,
		portableKey:         strconv.FormatBool(portable),
	}
}

func cephVolumeEnvVar() []v1.EnvVar {
	return []v1.EnvVar{
		{Name: "CEPH_VOLUME_DEBUG", Value: "1"},
		{Name: "CEPH_VOLUME_SKIP_RESTORECON", Value: "1"},
		// LVM will avoid interaction with udev.
		// LVM will manage the relevant nodes in /dev directly.
		{Name: "DM_DISABLE_UDEV", Value: "1"},
	}
}

func osdActivateEnvVar() []v1.EnvVar {
	monEnvVars := []v1.EnvVar{
		{Name: "ROOK_CEPH_MON_HOST",
			ValueFrom: &v1.EnvVarSource{
				SecretKeyRef: &v1.SecretKeySelector{LocalObjectReference: v1.LocalObjectReference{
					Name: "rook-ceph-config"},
					Key: "mon_host"}}},
		{Name: "CEPH_ARGS", Value: "-m $(ROOK_CEPH_MON_HOST)"},
	}

	return append(cephVolumeEnvVar(), monEnvVars...)
}

func (c *Cluster) osdRunFlagTuningOnPVC(osdID int) error {
	who := fmt.Sprintf("osd.%d", osdID)
	do := make(map[string]string)

	// Time in seconds to sleep before next recovery or backfill op
	do["osd_recovery_sleep"] = osdRecoverySleep
	// Time in seconds to sleep before next snap trim
	do["osd_snap_trim_sleep"] = osdSnapTrimSleep
	// Time in seconds to sleep before next removal transaction
	do["osd_delete_sleep"] = osdDeleteSleep

	monStore := opconfig.GetMonStore(c.context, c.clusterInfo)

	for flag, val := range do {
		err := monStore.Set(who, flag, val)
		if err != nil {
			return errors.Wrapf(err, "failed to set %q to %q on %q", flag, val, who)
		}
	}

	return nil
}

func getUdevVolume() (v1.Volume, v1.VolumeMount) {
	volume := v1.Volume{
		Name: udevVolName,
		VolumeSource: v1.VolumeSource{
			HostPath: &v1.HostPathVolumeSource{Path: udevPath},
		},
	}

	volumeMounts := v1.VolumeMount{
		Name:      udevVolName,
		MountPath: udevPath,
	}

	return volume, volumeMounts
}

// PrivilegedContext returns a privileged Pod security context
func PrivilegedContext() *v1.SecurityContext {
	privileged := true

	return &v1.SecurityContext{
		Privileged: &privileged,
	}
}
