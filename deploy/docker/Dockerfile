# Distroless: no shell, no package manager, no libc beyond what a static Go
# binary needs — which is none, because CGO is off (ADR-0004's pure-Go SQLite
# driver is what makes that possible; a cgo driver would drag in libc and this
# image would have to be debian-based).
#
# `nonroot` runs as uid 65532. Heyarr needs no capabilities and never changes
# ownership, so there is nothing for root to do here (§74).
#
# goreleaser supplies the already-built binary; this image never compiles
# anything, so what ships is bit-identical to the released archive rather than
# a second build that happens to look similar.
FROM gcr.io/distroless/static-debian12:nonroot

COPY heyarr /usr/local/bin/heyarr

# The data directory must be a volume or a bind mount. The image is read-only
# apart from it, which is the point.
VOLUME ["/var/lib/heyarr"]

# Loopback by default (ADR-0011). Publishing this port without also setting
# http.auth.enabled is refused by the binary itself, not merely warned about.
EXPOSE 7777

USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/heyarr"]
CMD ["--config", "/etc/heyarr/config.yaml", "all"]
