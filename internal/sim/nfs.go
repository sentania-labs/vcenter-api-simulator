package sim

import (
	"fmt"

	"github.com/sentania/vcenter-api-simulator/internal/topology"
	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vim25/types"
)

// createNFS adds the shared NFS v3 datastore in registry serial order.
func (s *Simulator) createNFS(ctx *simulator.Context, dc *topology.Datacenter, n topology.NFSDatastore) (*simulator.Datastore, error) {
	spec := n.Spec
	volume := &types.HostNasVolume{HostFileSystemVolume: types.HostFileSystemVolume{Type: "NFS", Name: spec.Name, Capacity: spec.Capacity}, RemoteHost: spec.RemoteHost, RemoteHostNames: []string{spec.RemoteHost}, RemotePath: spec.RemotePath, ProtocolEndpoint: types.NewBool(false)}
	info := &types.NasDatastoreInfo{DatastoreInfo: types.DatastoreInfo{FreeSpace: spec.FreeSpace}, Nas: volume}
	ds, err := s.createDatastore(ctx, s.dcs[dc], spec.Name, info, "NFS")
	if err != nil {
		return nil, err
	}
	ds.Summary.Capacity = spec.Capacity
	ds.Summary.FreeSpace = spec.FreeSpace
	ds.Summary.MultipleHostAccess = types.NewBool(true)
	ds.Summary.Url = fmt.Sprintf("ds:///vmfs/volumes/%s/", n.UUID)
	ds.Capability = types.DatastoreCapability{DirectoryHierarchySupported: true, PerFileThinProvisioningSupported: true, TopLevelDirectoryCreateSupported: types.NewBool(true), SeSparseSupported: types.NewBool(true), VmfsSparseSupported: types.NewBool(true), VmdkExpandSupported: types.NewBool(true)}
	return ds, nil
}

func (s *Simulator) mountNFS(ctx *simulator.Context, dc *topology.Datacenter, n topology.NFSDatastore, ds *simulator.Datastore) {
	volume := ds.Info.(*types.NasDatastoreInfo).Nas
	path := "/vmfs/volumes/" + n.UUID
	for _, refs := range s.Clusters {
		if refs.Cluster.Datacenter != dc {
			continue
		}
		ccr := ctx.Map.Get(refs.Ref).(*simulator.ClusterComputeResource)
		ctx.Map.AddReference(ctx, ccr, &ccr.Datastore, ds.Self)
		for _, ref := range refs.Hosts {
			host := ctx.Map.Get(ref).(*simulator.HostSystem)
			mount(ctx, host, ds, path)
			host.Config.FileSystemVolume.MountInfo = append(host.Config.FileSystemVolume.MountInfo, types.HostFileSystemMountInfo{MountInfo: types.HostMountInfo{Path: path, AccessMode: "readWrite", Mounted: types.NewBool(true), Accessible: types.NewBool(true)}, Volume: volume, VStorageSupport: "vStorageUnsupported"})
		}
	}
}
