// Copyright 2014 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

package gce

import (
	"code.google.com/p/google-api-go-client/compute/v1"
	"github.com/juju/errors"

	"github.com/juju/juju/constraints"
	"github.com/juju/juju/environs"
	"github.com/juju/juju/environs/imagemetadata"
	"github.com/juju/juju/environs/instances"
	"github.com/juju/juju/environs/simplestreams"
	"github.com/juju/juju/instance"
	"github.com/juju/juju/provider/common"
	"github.com/juju/juju/state/multiwatcher"
	"github.com/juju/juju/tools"
)

func (env *environ) StartInstance(args environs.StartInstanceParams) (*environs.StartInstanceResult, error) {
	// Please note that in order to fulfil the demands made of Instances and
	// AllInstances, it is imperative that some environment feature be used to
	// keep track of which instances were actually started by juju.
	env = env.getSnapshot()

	// Start a new raw instance.

	if args.MachineConfig.HasNetworks() {
		return nil, errors.New("starting instances with networks is not supported yet")
	}

	spec, err := env.finishMachineConfig(args)
	if err != nil {
		return nil, errors.Trace(err)
	}

	raw, err := env.newRawInstance(args, spec)
	if err != nil {
		return nil, errors.Trace(err)
	}

	inst := newInstance(raw, env)
	logger.Infof("started instance %q in zone %q", inst.Id(), inst.zone)

	// Build the result.

	hwc := env.getHardwareCharacteristics(spec, raw)

	result := environs.StartInstanceResult{
		Instance: inst,
		Hardware: hwc,
	}
	return &result, nil
}

func (env *environ) finishMachineConfig(args environs.StartInstanceParams) (*instances.InstanceSpec, error) {
	arches := args.Tools.Arches()
	series := args.Tools.OneSeries()
	spec, err := env.findInstanceSpec(env.Config().ImageStream(), &instances.InstanceConstraint{
		Region:      env.ecfg.region(),
		Series:      series,
		Arches:      arches,
		Constraints: args.Constraints,
	})
	if err != nil {
		return nil, errors.Trace(err)
	}

	envTools, err := args.Tools.Match(tools.Filter{Arch: spec.Image.Arch})
	if err != nil {
		return nil, errors.Errorf("chosen architecture %v not present in %v", spec.Image.Arch, arches)
	}

	args.MachineConfig.Tools = envTools[0]
	err = environs.FinishMachineConfig(args.MachineConfig, env.Config())
	return spec, errors.Trace(err)
}

func (env *environ) findInstanceSpec(stream string, ic *instances.InstanceConstraint) (*instances.InstanceSpec, error) {
	sources, err := environs.ImageMetadataSources(env)
	if err != nil {
		return nil, errors.Trace(err)
	}

	cloudSpec, err := env.cloudSpec(ic.Region)
	if err != nil {
		return nil, errors.Trace(err)
	}

	imageConstraint := imagemetadata.NewImageConstraint(simplestreams.LookupParams{
		CloudSpec: cloudSpec,
		Series:    []string{ic.Series},
		Arches:    ic.Arches,
		Stream:    stream,
	})

	matchingImages, _, err := imagemetadata.Fetch(sources, imageConstraint, signedImageDataOnly)
	if err != nil {
		return nil, errors.Trace(err)
	}

	images := instances.ImageMetadataToImages(matchingImages)
	spec, err := instances.FindInstanceSpec(images, ic, allInstanceTypes)
	return spec, errors.Trace(err)
}

func (env *environ) newRawInstance(args environs.StartInstanceParams, spec *instances.InstanceSpec) (*compute.Instance, error) {
	userData, err := environs.ComposeUserData(args.MachineConfig, nil)
	if err != nil {
		return nil, errors.Annotate(err, "cannot make user data")
	}
	logger.Debugf("GCE user data; %d bytes", len(userData))

	// Handle state machines.
	var role string
	if multiwatcher.AnyJobNeedsState(args.MachineConfig.Jobs...) {
		role = roleState
	}

	// Compose the instance.
	machineID := common.MachineFullName(env, args.MachineConfig.MachineId)
	disks := getDisks(spec, args.Constraints)
	networkInterfaces := getNetworkInterfaces()
	authKeys, err := gceSSHKeys()
	if err != nil {
		return nil, errors.Trace(err)
	}

	metadata := packMetadata(map[string]string{
		metadataKeyRole: role,
		// We store a snapshot of what information was used to create
		// this instance. It is only informational.
		metadataKeyCloudInit: string(userData),
		metadataKeySSHKeys:   authKeys,
	})

	instance := &compute.Instance{
		// MachineType is set in the env.gce.newInstance call.
		Name:              machineID,
		Disks:             disks,
		NetworkInterfaces: networkInterfaces,
		Metadata:          metadata,
	}

	availabilityZones, err := env.parseAvailabilityZones(args)
	if err != nil {
		return nil, errors.Trace(err)
	}
	// TODO(ericsnow) Is there a better way to do this?
	diskInit := instance.Disks[0].InitializeParams
	if err := env.gce.addInstance(instance, spec.InstanceType.Name, availabilityZones); err != nil {
		return nil, errors.Trace(err)
	}
	if instance.Disks[0].InitializeParams == nil {
		instance.Disks[0].InitializeParams = diskInit
	}
	return instance, nil
}

func getDisks(spec *instances.InstanceSpec, cons constraints.Value) []*compute.AttachedDisk {
	size := common.MinRootDiskSizeGiB
	if cons.RootDisk != nil && *cons.RootDisk > uint64(size) {
		size = int64(common.MiBToGiB(*cons.RootDisk))
	}
	dSpec := diskSpec{
		sizeHint:   size,
		imageURL:   imageBasePath + spec.Image.Id,
		boot:       true,
		autoDelete: true,
	}
	rootDisk := dSpec.newAttached()
	if cons.RootDisk != nil && dSpec.size() == int64(minDiskSize) {
		msg := "Ignoring root-disk constraint of %dM because it is smaller than the GCE image size of %dM"
		logger.Infof(msg, *cons.RootDisk, minDiskSize)
	}
	return []*compute.AttachedDisk{rootDisk}
}

func getNetworkInterfaces() []*compute.NetworkInterface {
	// TODO(ericsnow) Use the env ID for the network name
	// (instead of default)?
	// TODO(ericsnow) Make the network name configurable?
	// TODO(ericsnow) Support multiple networks?
	spec := networkSpec{}
	// TODO(ericsnow) Use a different name? Configurable?
	rootIF := spec.newInterface("External NAT")
	return []*compute.NetworkInterface{rootIF}
}

func (env *environ) getHardwareCharacteristics(spec *instances.InstanceSpec, raw *compute.Instance) *instance.HardwareCharacteristics {
	rawSize := raw.Disks[0].InitializeParams.DiskSizeGb
	rootDiskSize := uint64(rawSize) * 1024
	hwc := instance.HardwareCharacteristics{
		Arch:     &spec.Image.Arch,
		Mem:      &spec.InstanceType.Mem,
		CpuCores: &spec.InstanceType.CpuCores,
		CpuPower: spec.InstanceType.CpuPower,
		RootDisk: &rootDiskSize,
		// Tags: not supported in GCE.
		AvailabilityZone: &raw.Zone,
	}
	return &hwc
}

func (env *environ) AllInstances() ([]instance.Instance, error) {
	instances, err := env.instances()
	return instances, errors.Trace(err)
}

func (env *environ) StopInstances(instances ...instance.Id) error {
	env = env.getSnapshot()

	var ids []string
	for _, id := range instances {
		ids = append(ids, string(id))
	}
	err := env.gce.removeInstances(env, ids...)
	return errors.Trace(err)
}
