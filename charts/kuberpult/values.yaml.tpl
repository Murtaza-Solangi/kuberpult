# Default values for ..
# This is a YAML-formatted file.
# Declare variables to be passed into your templates.

git:
  url:  # git@github.com/.../...
  branch: "master"
  sourceRepoUrl: ""

hub: europe-west3-docker.pkg.dev/fdc-public-docker-registry/kuberpult

log:
  # Possible values are "gcp" for a gcp-optimized format and "default" for json
  format: ""
  # Other possible values are "DEBUG", "INFO", "ERROR"
  level: "WARN"
cd:
  image: kuberpult-cd-service
# In MOST cases, do NOT overwrite the "tag".
# In general, kuberpult only guarantees that running with the same version of frontend and cd service will work.
# For testing purposes, we allow to overwrite the tags individually, to test an old frontend service with a new cd service.
  tag: "$VERSION"
  backendConfig:
    create: false   # Add backend config for health checks on GKE only
    timeoutSec: 300  # 30sec is the default on gcp loadbalancers, however kuberpult needs more with parallel requests. It is the time how long the loadbalancer waits for kuberpult to finish calls to the rest endpoint "release"
  resources:
    limits:
      cpu: 2
      memory: 3Gi
    requests:
      cpu: 2
      memory: 3Gi
  enableSqlite: true
frontend:
  image: kuberpult-frontend-service
# In MOST cases, do NOT overwrite the "tag".
# In general, kuberpult only guarantees that running with the same version of frontend and cd service will work.
# For testing purposes, we allow to overwrite the tags individually, to test an old frontend service with a new cd service.
  tag: "$VERSION"
  resources:
    limits:
      cpu: 500m
      memory: 250Mi
    requests:
      cpu: 500m
      memory: 250Mi
ingress:
  create: true
  annotations: {}
  domainName: null
  exposeReleaseEndpoint: false
  iap:
    enabled: false
    secretName: null
  tls:
    host: null
    secretName: kuberpult-tls-secret
ssh:
  identity: |
    -----BEGIN OPENSSH PRIVATE KEY-----
    -----END OPENSSH PRIVATE KEY-----
  known_hosts: |
    github.com ecdsa-sha2-nistp256 AAAAE2VjZHNhLXNoYTItbmlzdHAyNTYAAAAIbmlzdHAyNTYAAABBBEmKSENjQEezOmxkZMy7opKgwFB9nkt5YRrYMjNuG5N87uRgg6CLrbo5wAdT/y6v0mKV0U2w0WZ2YB/++Tpockg=
pgp:
  keyRing: null

argocd:
  baseUrl: ""

datadogTracing:
  enabled: false
  debugging: false
  environment: "shared"

dogstatsdMetrics:
  enabled: false
  #  dogstatsD listens on port udp:8125 by default.
  #  https://docs.datadoghq.com/developers/dogstatsd/?tab=hostagent#agent
  #  datadog.dogstatsd.socketPath -- Path to the DogStatsD socket
  address: unix:///var/run/datadog/dsd.socket
  # datadog.dogstatsd.hostSocketPath -- Host path to the DogStatsD socket
  hostSocketPath: /var/run/datadog

imagePullSecrets: []

gke:
  backend_service_id: ""
  project_number: ""

environment_configs:
  bootstrap_mode: false
  # environment_configs_json: |
  #   {
  #     "production": {
  #       "upstream": {
  #           "latest": true
  #        },
  #        "argocd" :{}
  #     }
  #   }
  environment_configs_json: null

auth:
  azureAuth:
    enabled: false
    cloudInstance: "https://login.microsoftonline.com/"
    clientId: ""
    tenantId: ""
