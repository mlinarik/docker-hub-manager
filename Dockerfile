# syntax=docker/dockerfile:1
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
COPY web ./web
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /docker-tracker .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /docker-tracker /docker-tracker
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/docker-tracker"]

