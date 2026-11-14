FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG REVISION=unknown
ARG BRANCH=unknown
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w \
      -X github.com/prometheus/common/version.Version=${VERSION} \
      -X github.com/prometheus/common/version.Revision=${REVISION} \
      -X github.com/prometheus/common/version.Branch=${BRANCH}" \
    -o /amber_exporter .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /amber_exporter /bin/amber_exporter
EXPOSE 10062
USER nonroot
ENTRYPOINT ["/bin/amber_exporter"]
