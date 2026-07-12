# Sandbox runtime image: prebuilt static Go binaries + shared test assets.
# Binaries are cross-compiled on the host by docker/build.sh (run before `up`).
FROM ubuntu:22.04
COPY docker/stage/room docker/stage/lan /usr/local/bin/
COPY testdata /testdata
RUN chmod +x /usr/local/bin/room /usr/local/bin/lan
WORKDIR /data
