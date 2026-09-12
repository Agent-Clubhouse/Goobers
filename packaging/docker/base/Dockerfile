# Linux base-image build inputs are prepared by `go run ./release
# -image-contexts <directory> -targets linux/amd64,...` with release metadata.
# Build with the context's explicit --platform; no source or credentials belong
# in this context. See README.md for the remaining image-publishing work.
ARG AZURE_LINUX_IMAGE=mcr.microsoft.com/azurelinux/base/core:3.0@sha256:daa1142fc6b44e27c8112ec6b4c2d579ddb9bc6b3747504e666010a45a51faa4
FROM ${AZURE_LINUX_IMAGE} AS runtime-files
# RPM post-install scripts run inside the installroot and need a real /dev/null.
# Remove the temporary device before copying: the container runtime supplies /dev.
RUN mkdir -p /rootfs/dev && mknod -m 666 /rootfs/dev/null c 1 3 \
    && tdnf install -y --releasever=3.0 --installroot /rootfs ca-certificates git tzdata bash \
    && rm -rf /rootfs/var/cache /rootfs/var/log /rootfs/etc/tdnf \
    && find /rootfs -type f -perm /6000 -delete \
    && rm /rootfs/dev/null \
    && mkdir -p /rootfs/home/nonroot /rootfs/instance /rootfs/tmp \
    && printf 'nonroot:x:65532:65532:Goobers:/home/nonroot:/bin/sh\n' > /rootfs/etc/passwd \
    && printf 'nonroot:x:65532:\n' > /rootfs/etc/group \
    && chown 65532:65532 /rootfs/home/nonroot /rootfs/instance \
    && chmod 1777 /rootfs/tmp

FROM scratch
COPY --from=runtime-files /rootfs /
COPY --chmod=755 goobers goobers-operator /usr/local/bin/
COPY release.json /usr/share/goobers/release.json
ENV PATH=/usr/local/bin:/usr/bin:/bin HOME=/home/nonroot
USER 65532:65532
WORKDIR /instance
# Check the shell, certificates, git, timezone data, writable HOME, and both
# executable/platform contracts during the build, with the final non-root user.
RUN /bin/sh -ec 'git --version; test -s /etc/pki/tls/certs/ca-bundle.crt; test -f /usr/share/zoneinfo/America/Los_Angeles; test -w "$HOME"; goobers --version; goobers-operator --version'
ENTRYPOINT ["goobers"]
CMD ["up"]
