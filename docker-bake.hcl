variable "REGISTRY" {
  default = "ghcr.io"
}

variable "OWNER" {
  default = "techarohq"
}

variable "VERSION" {
  default = "devel"
}

group "default" {
  targets = ["caddy", "reputationdbd"]
}

target "caddy" {
  context    = "."
  dockerfile = "docker/caddy-maat.Dockerfile"
  platforms  = ["linux/amd64", "linux/arm64"]
  tags = [
    "${REGISTRY}/${OWNER}/caddy-maat:${VERSION}",
  ]
}

target "reputationdbd" {
  context    = "."
  dockerfile = "docker/reputationdbd.Dockerfile"
  platforms  = ["linux/amd64", "linux/arm64"]
  tags = [
    "${REGISTRY}/${OWNER}/reputationdbd:${VERSION}",
  ]
}

# Single-architecture build for the host, so `docker buildx bake local --load`
# gives you something you can actually `docker run` without a multi-arch capable
# image store.
target "local" {
  inherits  = ["reputationdbd"]
  platforms = []
  tags      = ["reputationdbd:${VERSION}"]
}
