# Test-only model substitute; production worker/dispatcher binaries are unchanged.
FROM goobers:ha-nightly
USER root
COPY ca.crt /usr/local/share/ca-certificates/child-qualification.crt
RUN update-ca-certificates
COPY generated-child.yaml /usr/local/share/kind-child/generated-child.yaml
COPY model.cjs /usr/local/bin/claude
RUN chmod 0755 /usr/local/bin/claude
USER 65532:65532
