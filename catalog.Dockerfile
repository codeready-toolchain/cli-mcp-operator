# File-based catalog (FBC). Built by `make catalog-build` from `opm render`.
FROM quay.io/operator-framework/opm:v1.59.0

ENTRYPOINT ["/bin/opm"]
CMD ["serve", "/configs", "--cache-dir=/tmp/cache"]

ADD catalog /configs

LABEL operators.operatorframework.io.index.configs.v1=/configs
