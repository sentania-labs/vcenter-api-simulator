package control

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/sentania/vcenter-api-simulator/internal/credentials"
	"github.com/sentania/vcenter-api-simulator/internal/serve"
)

// Security: how long an idle tool stays signed in, the certificate the
// simulator shows tools, and the console's own password.

func init() {
	tasks["idle"] = taskDef{form: formIdle, plan: planIdle}
	tasks["new-certificate"] = taskDef{form: formNewCertificate, plan: planNewCertificate}
	tasks["own-certificate"] = taskDef{form: formOwnCertificate, plan: planOwnCertificate}
	tasks["console-password"] = taskDef{form: formConsolePassword, plan: planConsolePassword, now: true}
}

// idleChoices are the idle sign-out times the console offers, in order.
var idleChoices = []struct {
	Label string
	After time.Duration
}{{"Never", 0}, {"15 minutes", 15 * time.Minute}, {"30 minutes", 30 * time.Minute}, {"1 hour", time.Hour}, {"8 hours", 8 * time.Hour}, {"1 day", 24 * time.Hour}}

// idleLabel says an idle time in words; a time that is not one of the
// choices (set when the simulator was installed) is spelled out.
func idleLabel(d time.Duration) string {
	for _, ch := range idleChoices {
		if ch.After == d {
			return ch.Label
		}
	}
	switch {
	case d%time.Hour == 0:
		return plural(int(d/time.Hour), "hour")
	case d%time.Minute == 0:
		return plural(int(d/time.Minute), "minute")
	}
	return plural(int(d/time.Second), "second")
}

type idleParams struct {
	Idle string `json:"idle"`
}

func formIdle(c *Controller, _ map[string]string, _ Caller) (*Form, error) {
	cur := idleLabel(c.opts.SessionTimeout)
	now := "tools are never signed out for being idle"
	if c.opts.SessionTimeout > 0 {
		now = "tools are signed out after " + cur + " of no activity"
	}
	var labels []string
	for _, ch := range idleChoices {
		labels = append(labels, ch.Label)
	}
	return &Form{Title: "Change when idle tools are signed out", Crumbs: [][2]string{{"Security", "/security"}},
		Intro:   "Also called the session timeout. Now: " + now + ".",
		Values:  map[string]any{"idle": cur},
		Options: map[string]any{"choices": labels, "current": cur}}, nil
}

func planIdle(c *Controller, raw json.RawMessage, _ Caller) (*Plan, error) {
	var in idleParams
	if err := decodeParams(raw, &in); err != nil {
		return nil, err
	}
	p := &Plan{Go: "Change the time limit", dest: "/security"}
	after, found := time.Duration(0), false
	for _, ch := range idleChoices {
		if ch.Label == in.Idle {
			after, found = ch.After, true
		}
	}
	cur := idleLabel(c.opts.SessionTimeout)
	if !found {
		p.bad("idle", "Choose a time from the list.")
		p.Idle = "Nothing is different yet. Choose another time and this box will say what happens."
		return p, nil
	}
	if after == c.opts.SessionTimeout {
		p.bad("idle", "It is already set to %s. Choose a different time, or press Cancel.", strings.ToLower(cur))
		p.Idle = "Nothing is different yet. Choose another time and this box will say what happens."
		return p, nil
	}
	if after == 0 {
		p.Sum = append(p.Sum, say("Tools are %s signed out for being idle.", bold("never")))
	} else {
		p.Sum = append(p.Sum, say("A tool that asks for nothing for %s is signed out, and has to sign in again.", bold(in.Idle)))
	}
	p.Sum = append(p.Sum, say("It applies to tools that are already signed in as well as new ones."),
		note("A tool that collects every few minutes is never quiet that long, so it is not affected."))
	p.text = "Change the idle sign-out to " + strings.ToLower(in.Idle)
	p.apply = func() (*Outcome, error) {
		if err := c.setSessionTimeout(after); err != nil {
			return nil, err
		}
		if after == 0 {
			return &Outcome{Done: "Tools are never signed out for being idle."}, nil
		}
		return &Outcome{Done: fmt.Sprintf("Tools are signed out after %s of no activity.", in.Idle)}, nil
	}
	return p, nil
}

// ---------------------------------------------------------------------------
// certificate

type certParams struct {
	Names   string `json:"names"`
	Crt     string `json:"crt"`
	Key     string `json:"key"`
	CrtName string `json:"crtName"`
	KeyName string `json:"keyName"`
}

func (c *Controller) certInfo() (serve.CertificateInfo, error) {
	if c.opts.Certificate == nil {
		return serve.CertificateInfo{}, invalid("The certificate cannot be changed from the console in this installation.")
	}
	return c.opts.Certificate.Info(), nil
}

const managedCertificate = "This certificate comes from the settings the simulator was installed with, so it cannot be changed here."

func formNewCertificate(c *Controller, _ map[string]string, who Caller) (*Form, error) {
	info, err := c.certInfo()
	if err != nil {
		return nil, err
	}
	if info.Managed {
		return nil, invalid(managedCertificate)
	}
	// the name of this machine and the address the console was opened
	// with, then the names the current certificate already covers
	var names []string
	add := func(n string) {
		if n = strings.TrimSpace(n); n != "" && !slices.Contains(names, n) {
			names = append(names, n)
		}
	}
	add(c.opts.Endpoint)
	if host, _, err := net.SplitHostPort(who.Host); err == nil {
		add(host)
	} else {
		add(strings.Trim(who.Host, "[]"))
	}
	for _, n := range info.Names {
		add(n)
	}
	return &Form{Title: "Make a new certificate", Crumbs: [][2]string{{"Security", "/security"}}, Danger: true,
		Intro:   "The simulator makes itself a fresh certificate to replace the one it has.",
		NowNote: "Tools are signed out at that moment and refuse to reconnect until they trust the new certificate.",
		Values:  map[string]any{"names": strings.Join(names, ", ")}}, nil
}

func splitNames(s string) []string {
	var out []string
	for _, n := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ';' || r == '\n' }) {
		if n = strings.TrimSpace(n); n != "" && !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	return out
}

// replaceCertificate swaps the served certificate and ends every session.
// c.mu is held.
func (c *Controller) replaceCertificate(change func(*serve.Certificate) error) error {
	cert := c.opts.Certificate
	return c.applied().sim.UpdateCredentials(func() ([]string, error) {
		if err := change(cert); err != nil {
			return nil, err
		}
		return []string{"*"}, nil
	})
}

func planNewCertificate(c *Controller, raw json.RawMessage, _ Caller) (*Plan, error) {
	var in certParams
	if err := decodeParams(raw, &in); err != nil {
		return nil, err
	}
	info, err := c.certInfo()
	if err != nil {
		return nil, err
	}
	if info.Managed {
		return nil, invalid(managedCertificate)
	}
	p := &Plan{Go: "Replace the certificate", dest: "/security"}
	names := splitNames(in.Names)
	if len(names) == 0 {
		p.bad("names", "Enter at least one name or address, for example vcsim.lab.example.")
	}
	for _, n := range names {
		if strings.ContainsAny(n, " /\\\t") || len(n) > 253 {
			p.bad("names", "A name cannot contain spaces or slashes. Separate names with commas.")
		}
	}
	shown := strings.Join(names, ", ")
	if shown == "" {
		shown = "the names you enter"
	}
	p.Sum = []Item{
		say("A new certificate replaces the current one, valid for %s for 10 years.", bold(shown)),
		say("%s How to do that depends on the tool; most ask you to accept it again or paste its fingerprint.", bold("Tools will refuse to connect until you tell them to trust the new certificate.")),
		say("Tools that are signed in now are signed out."),
		note("The old certificate cannot be brought back.")}
	if len(p.Errors) > 0 {
		return p, nil
	}
	p.text = "Make a new certificate"
	p.apply = func() (*Outcome, error) {
		if err := c.replaceCertificate(func(cert *serve.Certificate) error { return cert.Regenerate(names) }); err != nil {
			if errors.Is(err, serve.ErrCertificateManaged) {
				return nil, invalid(managedCertificate)
			}
			return nil, err
		}
		c.tools.cut("", ToolCert, c.now(), c.paused.Load(), c.pausedAt)
		return &Outcome{Done: "New certificate made. Tools that were signed in have been signed out. They will not reconnect until you tell them to trust the new certificate; its fingerprint is on this screen.",
			history: "New certificate made. Tools that were signed in were signed out."}, nil
	}
	return p, nil
}

func formOwnCertificate(c *Controller, _ map[string]string, _ Caller) (*Form, error) {
	info, err := c.certInfo()
	if err != nil {
		return nil, err
	}
	if info.Managed {
		return nil, invalid(managedCertificate)
	}
	return &Form{Title: "Use my own certificate", Crumbs: [][2]string{{"Security", "/security"}}, Danger: true,
		Intro:   "Give the simulator a certificate from your own certificate authority, so tools trust it without extra steps.",
		NowNote: "Tools are signed out at that moment. Those that do not trust your certificate authority refuse to reconnect.",
		Values:  map[string]any{"crt": "", "key": "", "crtName": "", "keyName": ""}}, nil
}

// plainDate is a day as "3 March 2027".
func plainDate(t time.Time) string { return t.UTC().Format("2 January 2006") }

func planOwnCertificate(c *Controller, raw json.RawMessage, _ Caller) (*Plan, error) {
	var in certParams
	if err := decodeParams(raw, &in); err != nil {
		return nil, err
	}
	cur, err := c.certInfo()
	if err != nil {
		return nil, err
	}
	if cur.Managed {
		return nil, invalid(managedCertificate)
	}
	p := &Plan{Go: "Use this certificate", dest: "/security", Extra: map[string]any{}}
	const idle = "Choose both files and this box will say what happens."
	if in.Crt == "" {
		p.bad("crt", "Choose the certificate file.")
		if in.Key == "" {
			p.bad("key", "Choose the private key file.")
		}
		p.Idle = idle
		return p, nil
	}
	info, keyOK, err := serve.Inspect([]byte(in.Crt), []byte(in.Key))
	if err != nil {
		p.bad("crt", "That file does not hold a certificate. Choose the file that usually ends in .crt or .pem.")
		p.Extra["found"] = map[string]any{"state": "bad", "text": "That file does not hold a certificate."}
		p.Idle = idle
		return p, nil
	}
	names := strings.Join(info.Names, ", ")
	var dropped []string
	for _, n := range cur.Names {
		if !slices.Contains(info.Names, n) {
			dropped = append(dropped, n)
		}
	}
	issued := "issued by " + info.Issuer
	if info.SelfSigned || info.Issuer == "" {
		issued = "signed by itself"
	}
	found := map[string]any{"state": "", "text": fmt.Sprintf("valid for %s, %s, runs out %s.", names, issued, plainDate(info.Expires))}
	if len(dropped) > 0 {
		found["dropped"] = fmt.Sprintf("It does not cover %s, which the certificate in use now does, so a tool that connects by that address will be refused.", listAnd(dropped))
	}
	expired := !info.Expires.After(c.now())
	if expired {
		found["expired"] = "It has already run out, so tools will refuse it."
	}
	switch {
	case in.Key == "":
		p.bad("key", "Choose the private key file.")
		p.Idle = idle
	case !keyOK:
		found["state"] = "bad"
		p.bad("key", "This key does not belong to the certificate. Choose the key that was created together with it.")
		p.Idle = "This cannot go ahead yet: the key file does not belong to the certificate. Choose the right key."
	default:
		found["state"] = "ok"
	}
	p.Extra["found"] = found
	if expired {
		p.bad("crt", "This certificate ran out on %s. Choose one that is still valid.", plainDate(info.Expires))
	}
	if len(p.Errors) > 0 {
		return p, nil
	}
	p.Sum = append(p.Sum, say("Your certificate replaces the current one, valid for %s until %s.", bold(names), plainDate(info.Expires)))
	if info.SelfSigned || info.Issuer == "" {
		p.Sum = append(p.Sum, say("It is signed by itself, so every tool must be told to trust it before it reconnects."))
	} else {
		p.Sum = append(p.Sum, say("Tools that already trust %s connect without further steps. Others must be told to trust it.", info.Issuer))
	}
	if len(dropped) > 0 {
		p.Sum = append(p.Sum, say("Tools that connect by the address %s will be refused, because this certificate does not cover it.", listAnd(dropped)))
	}
	p.Sum = append(p.Sum, say("Tools that are signed in now are signed out."))
	p.text = "Use my own certificate"
	p.apply = func() (*Outcome, error) {
		if err := c.replaceCertificate(func(cert *serve.Certificate) error { return cert.Replace([]byte(in.Crt), []byte(in.Key)) }); err != nil {
			if errors.Is(err, serve.ErrCertificateManaged) {
				return nil, invalid(managedCertificate)
			}
			return nil, &Invalid{upper1(err.Error())}
		}
		if info.SelfSigned || info.Issuer == "" {
			c.tools.cut("", ToolCert, c.now(), c.paused.Load(), c.pausedAt)
			return &Outcome{Done: "The simulator now uses your certificate. Tools must be told to trust it before they reconnect.", history: "The simulator now uses your own certificate."}, nil
		}
		return &Outcome{Done: fmt.Sprintf("The simulator now uses your certificate. Tools that trust %s reconnect by themselves.", info.Issuer), history: "The simulator now uses your own certificate."}, nil
	}
	return p, nil
}

// ---------------------------------------------------------------------------
// console password

type consolePasswordParams struct {
	Cur string `json:"cur"`
	PW  string `json:"pw"`
	PW2 string `json:"pw2"`
}

const managedConsolePassword = "The console password comes from the settings the simulator was installed with, so it cannot be changed here."

// ErrWrongConsolePassword is returned by ConsolePassword when the current
// password given is not the right one.
var ErrWrongConsolePassword = errors.New("that is not the current password")

func formConsolePassword(c *Controller, _ map[string]string, _ Caller) (*Form, error) {
	if c.ConsolePassword == nil {
		return nil, invalid("The console password cannot be changed in this installation.")
	}
	if c.ConsoleManaged != nil && c.ConsoleManaged() {
		return nil, invalid(managedConsolePassword)
	}
	return &Form{Title: "Change console password", Crumbs: [][2]string{{"Security", "/security"}}, NoWhen: true,
		Values: map[string]any{"cur": "", "pw": "", "pw2": ""}}, nil
}

func planConsolePassword(c *Controller, raw json.RawMessage, who Caller) (*Plan, error) {
	var in consolePasswordParams
	if err := decodeParams(raw, &in); err != nil {
		return nil, err
	}
	if c.ConsolePassword == nil {
		return nil, invalid("The console password cannot be changed in this installation.")
	}
	if c.ConsoleManaged != nil && c.ConsoleManaged() {
		return nil, invalid(managedConsolePassword)
	}
	p := &Plan{Go: "Change password", dest: "/security", Sum: []Item{
		say("The console password changes straight away."),
		say("You stay signed in here. Anyone else signed in to the console is signed out."),
		note("Tools and their usernames are not affected.")}}
	if in.Cur == "" {
		p.bad("cur", "Type the password you use now.")
	}
	switch n := len([]rune(in.PW)); {
	case n < credentials.MinPassword:
		p.bad("pw", "Use at least %d characters.", credentials.MinPassword)
	case len(in.PW) > 72:
		p.bad("pw", "That password is too long. Use 72 characters or fewer.")
	case in.PW != in.PW2:
		p.bad("pw2", "The two new passwords are different.")
	}
	if len(p.Errors) > 0 {
		return p, nil
	}
	p.text = "Change the console password"
	p.apply = func() (*Outcome, error) {
		if err := c.ConsolePassword(in.Cur, in.PW, who.Session); err != nil {
			var inv *Invalid
			switch {
			case errors.Is(err, ErrWrongConsolePassword):
				return nil, &fieldError{"cur", "That is not the current password."}
			case errors.As(err, &inv):
				return nil, &fieldError{"cur", inv.Msg}
			}
			return nil, err
		}
		return &Outcome{Done: "The console password was changed. Use the new one next time you sign in.", history: "Console password changed."}, nil
	}
	return p, nil
}

// ---------------------------------------------------------------------------
// the screen

// CertificateView is the certificate as the Security screen shows it.
type CertificateView struct {
	// Kind is made (the simulator's own), own (uploaded) or managed
	// (supplied by the deployment).
	Kind   string `json:"kind"`
	Issuer string `json:"issuer,omitempty"`
	// SelfSigned: no authority issued it, so every tool must be told to
	// trust it.
	SelfSigned  bool      `json:"selfSigned"`
	Names       []string  `json:"names"`
	Expires     time.Time `json:"expires"`
	Days        int       `json:"days"`
	Fingerprint string    `json:"fingerprint"`
}

// SecurityView is the Security screen.
type SecurityView struct {
	// Idle is the idle sign-out in words; "" means never.
	Idle        string           `json:"idle"`
	Certificate *CertificateView `json:"certificate,omitempty"`
	// ConsoleManaged: the console password comes from the deployment.
	ConsoleManaged bool `json:"consoleManaged"`
}

func (c *Controller) certificateView() *CertificateView {
	if c.opts.Certificate == nil {
		return nil
	}
	info := c.opts.Certificate.Info()
	v := &CertificateView{Kind: "made", Issuer: info.Issuer, SelfSigned: info.SelfSigned, Names: info.Names, Expires: info.Expires, Fingerprint: fingerprint(info.Fingerprint),
		Days: int(info.Expires.Sub(c.now()).Hours() / 24)}
	if !info.Expires.After(c.now()) {
		v.Days = -1 // run out, however recently
	}
	switch {
	case info.Managed:
		v.Kind = "managed"
	case !info.SelfSigned || info.Issuer != serve.SelfSignedName:
		// uploaded, whether an authority issued it or it signed itself
		v.Kind = "own"
	}
	return v
}

// fingerprint writes a SHA-256 fingerprint the way tools show it: upper
// case, in pairs separated by colons.
func fingerprint(hexed string) string {
	hexed = strings.ToUpper(hexed)
	var parts []string
	for i := 0; i+2 <= len(hexed); i += 2 {
		parts = append(parts, hexed[i:i+2])
	}
	return strings.Join(parts, ":")
}

// Security returns the Security screen.
func (c *Controller) Security() SecurityView {
	c.mu.Lock()
	defer c.mu.Unlock()
	v := SecurityView{Certificate: c.certificateView(), ConsoleManaged: c.ConsoleManaged != nil && c.ConsoleManaged()}
	if c.opts.SessionTimeout > 0 {
		v.Idle = idleLabel(c.opts.SessionTimeout)
	}
	return v
}

// setSessionTimeout saves the idle sign-out and applies it to tools that
// are signed in already. c.mu is held.
func (c *Controller) setSessionTimeout(d time.Duration) error {
	b, err := json.Marshal(RuntimeSettings{SessionTimeoutSeconds: int64(d / time.Second)})
	if err != nil {
		return err
	}
	if err = os.MkdirAll(c.opts.DataDir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(c.opts.DataDir, ".runtime-settings-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), c.settingsPath()); err != nil {
		return err
	}
	c.opts.SessionTimeout = d
	c.applied().sim.SetSessionTimeout(d)
	return nil
}
