FROM sammobach/go:1.19 AS build
RUN mkdir /app
ADD . /app/
WORKDIR /app
RUN mkdir -p /app/bin && go build -ldflags "-s -w" -o /app/bin/s3-virus-scan .
CMD ["/app/bin/s3-virus-scan"]

FROM alpine:3.14.0
LABEL maintainer="Sam Mobach <s.mobach@marktmonitor.com>"
RUN mkdir /app
COPY --from=build /app /app
WORKDIR /app
RUN apk update && apk add --no-cache ca-certificates clamav clamav-libunrar && apk add --upgrade libcurl openssl && rm -rf /var/cache/apk/*
CMD ["/app/bin/s3-virus-scan"]