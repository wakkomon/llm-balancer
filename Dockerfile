FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
COPY web ./web
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /llm-balancer .

FROM scratch
COPY --from=build /llm-balancer /llm-balancer
EXPOSE 8080
USER 65534:65534
ENTRYPOINT ["/llm-balancer"]
