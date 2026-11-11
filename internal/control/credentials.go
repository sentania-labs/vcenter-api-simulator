package control

import "github.com/sentania/vcenter-api-simulator/internal/credentials"

type CredentialsView struct {
	Users    []credentials.User `json:"users"`
	AnyLogin bool               `json:"anyLogin"`
	Pending  bool               `json:"pending"`
}

func (c *Controller) credentialsView() CredentialsView {
	snapshot := c.auth.Snapshot()
	pending := false
	d := c.draft
	if d == nil && c.scheduled != nil {
		d = c.scheduled.Draft
	}
	if d != nil && d.Credentials != nil {
		snapshot = *d.Credentials
		pending = true
	}
	return CredentialsView{Users: snapshot.Users(), AnyLogin: snapshot.AnyLogin, Pending: pending}
}
func (c *Controller) CredentialsView() CredentialsView {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.credentialsView()
}
func (c *Controller) credentialDraft() (*credentials.Snapshot, error) {
	d := c.ensureDraft()
	if d == nil {
		return nil, invalid("a change is scheduled; cancel it before editing")
	}
	if d.Credentials == nil {
		s := c.auth.Snapshot()
		d.Credentials = &s
	}
	return d.Credentials, nil
}
func (c *Controller) editCredentials(edit func(*credentials.Snapshot) error) (CredentialsView, error) {
	if c.scheduled != nil {
		return CredentialsView{}, invalid("a change is scheduled; cancel it before editing")
	}
	var snapshot credentials.Snapshot
	if c.draft != nil && c.draft.Credentials != nil {
		snapshot = c.draft.Credentials.Copy()
	} else {
		snapshot = c.auth.Snapshot()
	}
	if err := edit(&snapshot); err != nil {
		return CredentialsView{}, &Invalid{err.Error()}
	}
	d := c.ensureDraft()
	d.Credentials = &snapshot
	return c.credentialsView(), nil
}
func (c *Controller) PutUser(name, password, role string) (CredentialsView, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.editCredentials(func(s *credentials.Snapshot) error { return s.Put(name, password, role) })
}
func (c *Controller) DeleteUser(name string) (CredentialsView, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.editCredentials(func(s *credentials.Snapshot) error { return s.Delete(name) })
}
func (c *Controller) SetAnyLogin(enabled bool) (CredentialsView, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	d, err := c.credentialDraft()
	if err != nil {
		return CredentialsView{}, err
	}
	d.AnyLogin = enabled
	c.settle()
	return c.credentialsView(), nil
}
