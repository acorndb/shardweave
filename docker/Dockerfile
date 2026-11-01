# Runtime-only image. `make e2e-up` cross-compiles bin/shardweave for linux
# first; building inside a golang image is avoided because the local Docker
# daemon cannot reach Docker Hub (only cached mysql:8.0 is available).
FROM scratch
COPY bin/shardweave /shardweave
ENTRYPOINT ["/shardweave"]
CMD ["-config", "/etc/shardweave/config.yaml"]
