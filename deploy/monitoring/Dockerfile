# Pinned by digest, not by tag (#4569). `python:3.12-slim` is a floating tag:
# it moves with every CPython patch release and every Debian security rebuild,
# so two builds of this file a week apart produced different operating systems
# and different interpreters with no record of the change. The digest below
# names exactly one multi-arch image index (linux/amd64 + linux/arm64), so a
# rebuild either reproduces the reviewed image or fails.
#
# Refreshing this pin is a deliberate, reviewed act — see README.md,
# "Dependency management".
ARG PYTHON_IMAGE=docker.io/library/python:3.12-slim@sha256:78387bc3881b8273120a12ebe6c1ab22b018ccc2c9adf565ae1ac9b536e184ea
FROM ${PYTHON_IMAGE}

# --require-hashes makes pip refuse any artifact whose sha256 is not listed in
# requirements.txt, and refuse any requirement that is not pinned with `==`.
# --no-deps is safe (and required for determinism) only because that file is a
# complete transitive closure; if a dependency is ever added without being
# locked, the build fails here rather than resolving it silently.
COPY requirements.txt /tmp/requirements.txt
RUN pip install --no-cache-dir --require-hashes --no-deps -r /tmp/requirements.txt \
	&& rm /tmp/requirements.txt

COPY exporter.py /app/exporter.py
EXPOSE 9779
ENTRYPOINT ["python3", "/app/exporter.py"]
