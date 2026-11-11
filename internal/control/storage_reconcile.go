package control

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/sentania/vcenter-api-simulator/internal/registry"
	"github.com/sentania/vcenter-api-simulator/internal/scenario"
)

// storageReferences tracks existing selectors so an ordinary draft edit does
// not silently forgive a newly entered typo. nil means recovery of saved state.
type storageReferences map[string]bool

func storageRefs(s *scenario.Scenario) storageReferences {
	refs := storageReferences{}
	for _, dc := range s.Datacenters {
		for _, n := range dc.NFS {
			prefix := dc.Name + "/nfs/" + n.Name
			for _, k := range n.Clusters {
				refs[prefix+"/cluster/"+k] = true
			}
			for _, h := range n.InaccessibleHosts {
				refs[prefix+"/host/"+h] = true
			}
		}
		for _, st := range dc.ISCSI {
			prefix := dc.Name + "/iscsi/" + st.Cluster
			refs[prefix] = true
			for _, l := range st.LUNs {
				for _, p := range l.Paths {
					if p.Host != "" {
						refs[prefix+"/lun/"+l.Name+"/"+p.Adapter+"/"+p.Host] = true
					}
				}
			}
		}
	}
	return refs
}

// reconcileStorage removes references that were valid before a topology edit
// and now point at removed objects. For saved-state recovery previous is nil,
// so any structurally valid dangling reference can be removed. An explicitly
// scoped datastore whose final cluster vanished is removed rather than widened.
func reconcileStorage(s *scenario.Scenario, previous *scenario.Scenario) (int, error) {
	base := clone(s)
	for i := range base.Datacenters {
		base.Datacenters[i].NFS = nil
		base.Datacenters[i].ISCSI = nil
	}
	if err := base.Validate(); err != nil {
		return 0, err
	}
	clusters := map[string]int{}
	r := registry.InMemory(base.Seed)
	for _, key := range base.ClusterKeys() {
		clusters[key] = base.Resolve(key, scenario.ClusterSpec{Hosts: base.Vary.DrawHosts(scenario.DrawSource(r.Sub(key)))}).Hosts
	}
	existing := storageReferences(nil)
	if previous != nil {
		existing = storageRefs(previous)
	}
	mayRemove := func(key string) bool { return previous == nil || existing[key] }
	validClusterKey := func(key, dc string) bool {
		prefix := dc + "/cluster-"
		if !strings.HasPrefix(key, prefix) {
			return false
		}
		suffix := strings.TrimPrefix(key, prefix)
		n, err := strconv.Atoi(suffix)
		return err == nil && n > 0 && fmt.Sprintf("cluster-%02d", n) == "cluster-"+suffix
	}
	hostCluster := func(key, dc string) (string, bool) {
		i := strings.LastIndexByte(key, '/')
		if i < 0 {
			return "", false
		}
		cluster, host := key[:i], key[i+1:]
		if !validClusterKey(cluster, dc) {
			return "", false
		}
		n, ok := scenario.StorageHostOrdinal(host)
		if !ok {
			return "", false
		}
		return cluster, clusters[cluster] >= n
	}
	removed := 0
	for di := range s.Datacenters {
		dc := &s.Datacenters[di]
		var nfs []scenario.NFSDatastore
		for _, n := range dc.NFS {
			prefix := dc.Name + "/nfs/" + n.Name
			scoped := len(n.Clusters) > 0
			var scopes []string
			for _, key := range n.Clusters {
				if validClusterKey(key, dc.Name) && clusters[key] == 0 && mayRemove(prefix+"/cluster/"+key) {
					removed++
					continue
				}
				scopes = append(scopes, key)
			}
			if scoped && len(scopes) == 0 {
				removed++
				continue
			}
			n.Clusters = scopes
			var hosts []string
			for _, h := range n.InaccessibleHosts {
				cluster, exists := hostCluster(h, dc.Name)
				inScope := !scoped || contains(scopes, cluster)
				if cluster != "" && (!exists || !inScope) && mayRemove(prefix+"/host/"+h) {
					removed++
					continue
				}
				hosts = append(hosts, h)
			}
			n.InaccessibleHosts = hosts
			nfs = append(nfs, n)
		}
		dc.NFS = nfs
		var stores []scenario.ISCSIStorage
		for _, st := range dc.ISCSI {
			prefix := dc.Name + "/iscsi/" + st.Cluster
			if validClusterKey(st.Cluster, dc.Name) && clusters[st.Cluster] == 0 && mayRemove(prefix) {
				removed++
				continue
			}
			var luns []scenario.ISCSILUN
			for _, l := range st.LUNs {
				var paths []scenario.ISCSIPath
				for _, p := range l.Paths {
					if p.Host != "" {
						cluster, exists := hostCluster(p.Host, dc.Name)
						if cluster == st.Cluster && !exists && mayRemove(prefix+"/lun/"+l.Name+"/"+p.Adapter+"/"+p.Host) {
							removed++
							continue
						}
					}
					paths = append(paths, p)
				}
				if len(l.Paths) > 0 && len(paths) == 0 {
					removed++
					continue
				}
				l.Paths = paths
				luns = append(luns, l)
			}
			st.LUNs = luns
			stores = append(stores, st)
		}
		dc.ISCSI = stores
	}
	return removed, nil
}
