# In-cluster probe image for verify scripts: curl, jq, python3.
FROM alpine:3.22@sha256:5291449c3df73caf6ed85e649dec1b9e818b39a5d8c871e97afc13e9cd5e8fa8
RUN apk add --no-cache curl jq python3 bash ca-certificates
USER 10001
ENTRYPOINT ["sleep", "infinity"]
