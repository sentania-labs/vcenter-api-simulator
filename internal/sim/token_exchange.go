package sim

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/vmware/govmomi/vim25/types"
)

// The ESX health client next exchanges its SAML assertion for an ESX-scoped
// access token on the vCenter API. This token is signed only by this simulator;
// the next connection is to ESX's own address, outside this vCenter service.
func (h *handler) exchangeToken(user string, spec map[string]any) (any, string) {
	text := func(key string) string { v, _ := optionalValue(spec[key]).(string); return v }
	if text("grant_type") != "urn:ietf:params:oauth:grant-type:token-exchange" || text("subject_token_type") != "urn:ietf:params:oauth:token-type:saml2" || text("requested_token_type") != "urn:ietf:params:oauth:token-type:id_token" || text("audience") != "vmware-tes:esx:auth" {
		return nil, "invalid_argument"
	}
	scope := text("scope")
	host, permission, ok := strings.Cut(scope, ",")
	if !ok || permission != "Host.Stats.Query" || !strings.HasPrefix(host, "HostSystem:") {
		return nil, "invalid_argument"
	}
	ref := types.ManagedObjectReference{Type: "HostSystem", Value: strings.TrimPrefix(host, "HostSystem:")}
	if h.registry.Get(ref) == nil {
		return nil, "not_found"
	}
	raw, err := base64.StdEncoding.DecodeString(text("subject_token"))
	if err != nil {
		return nil, "invalid_argument"
	}
	subject, valid := h.bearerUser(string(raw))
	if !valid || subject != user {
		return nil, "unauthenticated"
	}
	var assertion struct {
		Conditions struct {
			Until string `xml:"NotOnOrAfter,attr"`
		} `xml:"Conditions"`
	}
	if xml.Unmarshal(raw, &assertion) != nil {
		return nil, "invalid_argument"
	}
	until, err := time.Parse(time.RFC3339Nano, assertion.Conditions.Until)
	if err != nil {
		return nil, "invalid_argument"
	}
	now := h.env.Now().UTC()
	if limit := now.Add(5 * time.Minute); until.After(limit) {
		until = limit
	}
	material, err := h.issuer.Material()
	if err != nil {
		return nil, "internal_server_error"
	}
	encode := func(value any) string { b, _ := json.Marshal(value); return base64.RawURLEncoding.EncodeToString(b) }
	header := encode(map[string]any{"alg": "RS256", "typ": "JWT"})
	payload := encode(map[string]any{"iss": "vcenter-api-simulator", "sub": user, "aud": "vmware-tes:esx:auth", "scope": scope, "iat": now.Unix(), "exp": until.Unix(), "jti": uuid.NewString()})
	unsigned := header + "." + payload
	digest := sha256.Sum256([]byte(unsigned))
	signature, err := rsa.SignPKCS1v15(rand.Reader, material.Key, crypto.SHA256, digest[:])
	if err != nil {
		return nil, "internal_server_error"
	}
	return vapiStruct("com.vmware.oauth2.token_info", map[string]any{"access_token": unsigned + "." + base64.RawURLEncoding.EncodeToString(signature), "token_type": "Bearer", "expires_in": map[string]any{"OPTIONAL": int64(until.Sub(now).Seconds())}, "scope": map[string]any{"OPTIONAL": scope}, "issued_token_type": map[string]any{"OPTIONAL": "urn:ietf:params:oauth:token-type:id_token"}, "refresh_token": map[string]any{"OPTIONAL": nil}}), ""
}
