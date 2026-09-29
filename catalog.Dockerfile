# File-based catalog (FBC). Built by `make catalog-build` from `make catalog-render`.
# Pre-build the opm cache in the image. Serving with an empty --cache-dir makes
# opm 1.59 exit: integrity check failed .../digest: no such file or directory
# (staging CatalogSource CrashLoopBackOff).
FROM quay.io/operator-framework/opm:v1.59.0

COPY catalog /configs
RUN ["/bin/opm", "serve", "/configs", "--cache-dir=/tmp/cache", "--cache-only"]

ENTRYPOINT ["/bin/opm"]
CMD ["serve", "/configs", "--cache-dir=/tmp/cache"]

LABEL operators.operatorframework.io.index.configs.v1=/configs
