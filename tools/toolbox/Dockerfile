# In-cluster probe image for verify scripts: curl, jq, python3.
FROM alpine:3.22
RUN apk add --no-cache curl jq python3 bash ca-certificates
USER 10001
ENTRYPOINT ["sleep", "infinity"]
