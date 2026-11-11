package sim

import (
	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vim25/methods"
	"github.com/vmware/govmomi/vim25/soap"
	"github.com/vmware/govmomi/vim25/types"
)

type privilegeResponse struct {
	Returnval []bool `xml:"returnval"`
}
type privilegeBody struct {
	Res *privilegeResponse `xml:"HasPrivilegeOnEntityResponse"`
}

func (*privilegeBody) Fault() *soap.Fault { return nil }

type authorization struct {
	*simulator.AuthorizationManager
	role func(string) string
}

func privilegeSessionUser(ctx *simulator.Context, id string) (string, bool) {
	if id == "" || ctx.Session == nil {
		return "", false
	}
	sm, ok := ctx.Session.Get(ctx.Map.SessionManager().Reference()).(*simulator.SessionManager)
	if !ok {
		return "", false
	}
	for _, session := range sm.SessionList {
		if session.Key == id {
			return session.UserName, true
		}
	}
	return "", false
}
func (a *authorization) granted(user, priv string) bool {
	if a.role == nil {
		return true
	}
	switch a.role(user) {
	case "administrator":
		return true
	case "read-only":
		return priv == "System.Read" || priv == "System.View" || priv == "System.Anonymous"
	}
	return false
}
func (a *authorization) HasPrivilegeOnEntity(ctx *simulator.Context, req *types.HasPrivilegeOnEntity) soap.HasFault {
	user, active := privilegeSessionUser(ctx, req.SessionId)
	vals := make([]bool, len(req.PrivId))
	for i, id := range req.PrivId {
		vals[i] = active && a.granted(user, id)
	}
	return &privilegeBody{Res: &privilegeResponse{Returnval: vals}}
}
func (a *authorization) privileges(entities []types.ManagedObjectReference, user string, ids []string, active bool) []types.EntityPrivilege {
	out := make([]types.EntityPrivilege, 0, len(entities))
	for _, entity := range entities {
		p := types.EntityPrivilege{Entity: entity}
		for _, id := range ids {
			p.PrivAvailability = append(p.PrivAvailability, types.PrivilegeAvailability{PrivId: id, IsGranted: active && a.granted(user, id)})
		}
		out = append(out, p)
	}
	return out
}
func (a *authorization) HasPrivilegeOnEntities(ctx *simulator.Context, req *types.HasPrivilegeOnEntities) soap.HasFault {
	user, active := privilegeSessionUser(ctx, req.SessionId)
	return &methods.HasPrivilegeOnEntitiesBody{Res: &types.HasPrivilegeOnEntitiesResponse{Returnval: a.privileges(req.Entity, user, req.PrivId, active)}}
}
func (a *authorization) HasUserPrivilegeOnEntities(req *types.HasUserPrivilegeOnEntities) soap.HasFault {
	return &methods.HasUserPrivilegeOnEntitiesBody{Res: &types.HasUserPrivilegeOnEntitiesResponse{Returnval: a.privileges(req.Entities, req.UserName, req.PrivId, true)}}
}
func (a *authorization) FetchUserPrivilegeOnEntities(req *types.FetchUserPrivilegeOnEntities) soap.HasFault {
	if a.role == nil || a.role(req.UserName) == "administrator" {
		return a.AuthorizationManager.FetchUserPrivilegeOnEntities(req)
	}
	var out []types.UserPrivilegeResult
	for _, entity := range req.Entities {
		p := types.UserPrivilegeResult{Entity: entity}
		if a.role(req.UserName) == "read-only" {
			p.Privileges = []string{"System.Read", "System.View", "System.Anonymous"}
		}
		out = append(out, p)
	}
	return &methods.FetchUserPrivilegeOnEntitiesBody{Res: &types.FetchUserPrivilegeOnEntitiesResponse{Returnval: out}}
}
