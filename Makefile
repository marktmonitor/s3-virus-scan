.PHONY: all
all: build run

.PHONY: build
build:
	mkdir -p bin && go build -o bin/s3-virus-scan .

.PHONY: run
run:
	AWS_PROFILE=fwg LOG_LEVEL=debug ./bin/s3-virus-scan -c config.example.yaml

.PHONY: docker
docker:
	docker build --no-cache -t s3-virus-scan .