package control

import (
	"errors"
	"github.com/sentania/vcenter-api-simulator/internal/serve"
)

func (c *Controller) CertificateInfo() (serve.CertificateInfo, error) {
	if c.opts.Certificate == nil {
		return serve.CertificateInfo{}, errors.New("certificate settings unavailable")
	}
	return c.opts.Certificate.Info(), nil
}
func (c *Controller) changeCertificate(change func(*serve.Certificate) error) (serve.CertificateInfo, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cert := c.opts.Certificate
	if cert == nil {
		return serve.CertificateInfo{}, invalid("certificate settings unavailable")
	}
	err := c.applied().sim.UpdateCredentials(func() ([]string, error) {
		if err := change(cert); err != nil {
			return nil, err
		}
		return []string{"*"}, nil
	})
	if err != nil {
		if errors.Is(err, serve.ErrCertificateManaged) {
			return serve.CertificateInfo{}, invalid("certificate is managed by the deployment")
		}
		return serve.CertificateInfo{}, &Invalid{Msg: err.Error()}
	}
	return cert.Info(), nil
}
func (c *Controller) RegenerateCertificate(names []string) (serve.CertificateInfo, error) {
	return c.changeCertificate(func(cert *serve.Certificate) error { return cert.Regenerate(names) })
}
func (c *Controller) UploadCertificate(certPEM, keyPEM string) (serve.CertificateInfo, error) {
	return c.changeCertificate(func(cert *serve.Certificate) error { return cert.Replace([]byte(certPEM), []byte(keyPEM)) })
}
