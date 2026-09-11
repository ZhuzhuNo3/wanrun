FROM public.ecr.aws/aws-cli/aws-cli:2.36.21@sha256:fb09d91d0640088f15475d9a9366a215c88a24f9871ae0f6124bede0d2157e9a AS awscli
FROM quay.io/minio/minio:RELEASE.2025-09-07T16-13-09Z@sha256:14cea493d9a34af32f524e538b8346cf79f3321eff8e708c1e2960462bd8936e AS minio
FROM golang:1.25.4-bookworm@sha256:e17419604b6d1f9bc245694425f0ec9b1b53685c80850900a376fb10cb0f70cb

COPY --from=awscli /usr/local/aws-cli /usr/local/aws-cli
COPY --from=minio /usr/bin/minio /usr/bin/minio

RUN ln -s /usr/local/aws-cli/v2/current/bin/aws /usr/local/bin/aws
COPY scripts/private/install-root-test-dependencies.sh /usr/local/bin/install-root-test-dependencies
RUN /usr/local/bin/install-root-test-dependencies \
 && rm -f /usr/local/bin/install-root-test-dependencies

WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download \
 && go mod verify

COPY . /workspace
ARG TRANSFERLANES_DEVELOPMENT_COMMIT
ARG TRANSFERLANES_DEVELOPMENT_MODIFIED
RUN test ! -e /workspace/.git \
 && test -n "${TRANSFERLANES_DEVELOPMENT_COMMIT}" \
 && { test "${TRANSFERLANES_DEVELOPMENT_MODIFIED}" = true || test "${TRANSFERLANES_DEVELOPMENT_MODIFIED}" = false; }
ENV TRANSFERLANES_DEVELOPMENT_COMMIT=${TRANSFERLANES_DEVELOPMENT_COMMIT}
ENV TRANSFERLANES_DEVELOPMENT_MODIFIED=${TRANSFERLANES_DEVELOPMENT_MODIFIED}

ENTRYPOINT ["/workspace/test/default/run.sh"]
