package sim

import (
	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vim25/methods"
	"github.com/vmware/govmomi/vim25/soap"
	"github.com/vmware/govmomi/vim25/types"
)

// authorization preserves vcsim's privilege model while rejecting a session
// ID that is no longer active. User-specific permissions are not modelled.
type privilegeResponse struct {
	Returnval []bool `xml:"returnval"`
}
type privilegeBody struct {
	Res *privilegeResponse `xml:"HasPrivilegeOnEntityResponse"`
}

func (*privilegeBody) Fault() *soap.Fault { return nil }

type authorization struct {
	*simulator.AuthorizationManager
}

func activePrivilegeSession(ctx *simulator.Context, id string) bool {
	if id == "" || ctx.Session == nil {
		return false
	}
	sm, ok := ctx.Session.Get(ctx.Map.SessionManager().Reference()).(*simulator.SessionManager)
	if !ok {
		return false
	}
	for _, session := range sm.SessionList {
		if session.Key == id {
			return true
		}
	}
	return false
}

func (a *authorization) HasPrivilegeOnEntity(ctx *simulator.Context, req *types.HasPrivilegeOnEntity) soap.HasFault {
	if activePrivilegeSession(ctx, req.SessionId) {
		return a.AuthorizationManager.HasPrivilegeOnEntity(req)
	}
	return &privilegeBody{Res: &privilegeResponse{Returnval: make([]bool, len(req.PrivId))}}
}

func (a *authorization) HasPrivilegeOnEntities(ctx *simulator.Context, req *types.HasPrivilegeOnEntities) soap.HasFault {
	if activePrivilegeSession(ctx, req.SessionId) {
		return a.AuthorizationManager.HasPrivilegeOnEntities(req)
	}
	result := make([]types.EntityPrivilege, 0, len(req.Entity))
	for _, entity := range req.Entity {
		p := types.EntityPrivilege{Entity: entity}
		for _, id := range req.PrivId {
			p.PrivAvailability = append(p.PrivAvailability, types.PrivilegeAvailability{PrivId: id, IsGranted: false})
		}
		result = append(result, p)
	}
	return &methods.HasPrivilegeOnEntitiesBody{Res: &types.HasPrivilegeOnEntitiesResponse{Returnval: result}}
}
