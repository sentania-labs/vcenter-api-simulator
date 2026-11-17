# vCenter API Simulator Helm chart

Installs one simulated vCenter. Install the chart again under another release
name for another one, and give each its own `simulator.seed`: the same seed
presents the same vCenter identity.

The chart creates a Deployment (one replica, non-root, read-only root
filesystem, no added privileges), a Service on port 443, a ServiceAccount with
no token, and a volume for the simulator's state. A Secret, an Ingress and a
cert-manager Certificate are created only when you ask for them. It needs no
cluster-wide permissions, and nothing is reachable from outside the cluster
until you turn on `ingress`, `admin.service`, or a `LoadBalancer` or `NodePort`
Service.

## Install

```sh
helm install vcsim-01 charts/vcenter-api-simulator \
  --set hostname=vcsim-01.example.test \
  --set image.tag=latest
```

The chart's default image is the unreleased `main` build. Set `image.tag` to
a release such as `v0.4.5`, or to `latest`, and pin it in a deployment
repository.

Then, from your workstation:

```sh
kubectl port-forward svc/vcsim-01-vcenter-api-simulator 8443:443       # the vCenter
kubectl port-forward deploy/vcsim-01-vcenter-api-simulator 9090:9090   # the console
```

Open `http://127.0.0.1:9090/`. The first visit asks you to choose a console
password and then the password for `administrator@vsphere.local`. Point your
tool at `https://127.0.0.1:8443`. The notes `helm install` prints give the exact
names for your release.

With no `credentials` set there is no vCenter account until the console's
first visit creates one. To start with an account instead, create a Secret
with keys `username` and `password` and set `credentials.enabled=true` and
`credentials.existingSecret`. Set `credentials.role=read-only` for a
read-only account.

## Reach it from outside the cluster

Set `ingress.enabled`, `ingress.className`, and `ingress.tls.enabled` with
`ingress.tls.secretName`. The simulator speaks HTTPS, so most controllers need
to be told: for ingress-nginx add the annotation
`nginx.ingress.kubernetes.io/backend-protocol: "HTTPS"`. When the Ingress ends
TLS, tools see the Ingress's certificate, not the simulator's.

For VCF Operations' own vCenter adapter, two more settings are needed behind an
Ingress:

- Add `/lookupservice`, `/sso-adminserver`, `/sts`, `/ls`, `/api`, `/rest` and
  `/stats` to `ingress.paths`. The default is `/sdk` and `/vsanHealth` only.
- Set `simulator.lookupTrustSecret` to the Secret that holds the Ingress's
  certificate (only its `tls.crt` is used). The lookup service then tells
  Operations to trust that certificate at the sign-on addresses.

An example with all of this is in [`ci/ingress-values.yaml`](ci/ingress-values.yaml).

The console stays on the pod's own loopback. To reach it through the cluster,
set `admin.service.enabled` (a Service) or `admin.ingress.enabled`, add the
name it is reached by to `admin.allowedHosts`, and use TLS. Set
`admin.existingSecret` (a Secret with key `password`) to supply the console
password yourself.

## Certificates

In order of precedence:

1. `tls.existingSecret`: a `kubernetes.io/tls` Secret, served as is.
2. `tls.certManager.enabled`: a cert-manager Certificate for `hostname` (and
   `simulator.tlsNames`) from `tls.certManager.issuerRef`. A renewal changes the
   fingerprint a management pack pinned.
3. Neither: a self-signed certificate made on first start and kept in the
   volume.

A replaced certificate is served to the next connection without a restart.
A certificate supplied by the chart is read-only in the console.

## The state volume

`persistence.enabled` (on by default) keeps the identities, the certificate,
the console's settings and the request log across restarts. The claim survives
`helm uninstall` and Argo CD pruning. Without it every new pod makes new
identities and a new certificate, which breaks anything a tool pinned.

`persistence.size` defaults to 1Gi. That holds the state files and the request
log's cap (24 MiB). Larger inventories need more memory, not more disk: see
[sizing](../../docs/sizing.md).

### Upgrading

`helm upgrade` tries to grow an existing claim to `persistence.size`. Kubernetes
does that only when the storage class sets `allowVolumeExpansion: true`;
otherwise it rejects the whole upgrade. For an existing install, either:

- set `persistence.size` to the size the claim already has. The request log
  then limits itself to the space that is there; or
- expand the claim where the storage class allows it; or
- delete the claim so the next sync recreates it at the new size. Identities and
  the pinned certificate are made again too, so do this only when that is
  acceptable.

## Appliance networking

Some of Operations' reads ask the "appliance" for its hostname, DNS servers and
address. The hostname follows `simulator.publicUrl`, or `hostname`. DNS servers
and the address are the pod's own unless you set `simulator.applianceNetworking.dnsServers`
and `simulator.applianceNetworking.ipv4` (for example `10.20.0.10/24`). These
only change what is reported, not the pod's networking, and do not repair an
Operations DNS alert about the ingress address, which is a matter for your DNS.

## Argo CD

[`examples/argocd-application.yaml`](../../examples/argocd-application.yaml) is
one Application per simulated vCenter. Copy it and change the name, hostname and
seed. The Application needs only the right to create a Deployment, Service,
claim and Ingress in its namespace.

## Settings

Every setting, with its default and what it does, is commented in
[`values.yaml`](values.yaml). The ones that match a start-up flag are in the
[reference](../../docs/REFERENCE.md#start-up-flags-and-environment-variables):

| Chart value | Flag or variable |
|---|---|
| `simulator.seed` | `-seed` |
| `simulator.tlsNames` | `-tls-names` |
| `simulator.publicUrl` | `-public-url` |
| `simulator.lookupTrustSecret` | `-lookup-trust-cert` |
| `simulator.reportUrl` | `-report-url` |
| `simulator.applianceNetworking.*` | `-appliance-dns-servers`, `-appliance-ipv4` |
| `simulator.capture.*` | `-query-log`, `-query-log-max-*`, `-unanswered-stdout` |
| `simulator.extraArgs` | any other flag |
| `credentials.*` | `VCSIM_USERNAME`, `VCSIM_PASSWORD`, `VCSIM_ROLE` |
| `admin.existingSecret` | `VCSIM_ADMIN_PASSWORD` |
| `admin.allowedHosts` | `-admin-allowed-hosts` |
| `admin.port`, `admin.exposeToPodNetwork` | `-admin` |

## Probes

The chart uses `/readyz` for startup and readiness, and `/healthz` for liveness.
Startup waits up to five minutes for a large inventory to build. Details are in
[sizing](../../docs/sizing.md#probes).
