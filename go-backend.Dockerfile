# Go orchestrator backend. Builds a static Linux binary, then ships it in a
# static distroless runtime image. Build context is the repo root.
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -tags netgo,osusergo \
    -ldflags="-s -w -extldflags '-static'" \
    -o /server ./cmd/server
RUN ldd /server 2>&1 | grep -q "not a dynamic executable"

FROM gcr.io/distroless/static-debian12
COPY --from=build /server /server
# WORKDIR /app so the dashboard's os.Getwd()-relative reads (.context/people)
# resolve under the mounted /app tree, matching the Node dashboard's cwd.
WORKDIR /app
ENV DB_DIR=/app/db
ENTRYPOINT ["/server"]
