package sim

import (
	"net/url"
	"time"

	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vim25/types"
)

// installAuth makes vcsim check logins against the scenario's users. With
// no users vcsim keeps its default: any non-empty user name and password.
// A session idle timeout, when set, expires sessions the way a vCenter's
// config.vmacore.soap.sessionTimeout does.
func installAuth(m *simulator.Model, creds map[string]string, timeout time.Duration) {
	sm := m.Service.Context.Map.SessionManager()
	if len(creds) > 0 {
		// a non-default Listen.User switches SessionManager.Authenticate to
		// ValidLogin; the password in the URL is never used
		m.Service.Listen.User = url.UserPassword("vcsimulator", "")
		sm.ValidLogin = func(req *types.Login) bool {
			p, ok := creds[req.UserName]
			return ok && p == req.Password
		}
	}
	if timeout > 0 {
		om := m.Service.Context.Map.OptionManager()
		om.UpdateOptions(&types.UpdateOptions{ChangedValue: []types.BaseOptionValue{
			&types.OptionValue{Key: "config.vmacore.soap.sessionTimeout", Value: timeout.String()},
		}})
	}
}
