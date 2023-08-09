FROM golang:1.19-alpine

COPY ./qat_plugin /usr/bin/qat_plugin

ENTRYPOINT ["/usr/bin/qat_plugin"]