FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
COPY web ./web
ARG VERSION=dev
RUN mkdir -p /out/data
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /llm-balancer .

FROM scratch
COPY --from=build /llm-balancer /llm-balancer
# backends.json (changes made in the web UI) lives here – mount a volume to keep it across updates
COPY --from=build --chown=65534:65534 /out/data /data
ENV DATA_DIR=/data
VOLUME /data
EXPOSE 8080
USER 65534:65534
ENTRYPOINT ["/llm-balancer"]
