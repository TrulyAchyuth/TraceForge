FROM golang:1.25-alpine AS build
ENV GOTOOLCHAIN=auto
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -o /out/ ./cmd/...

FROM alpine:3.20
RUN adduser -D -u 10001 tf
COPY --from=build /out/ /usr/local/bin/
COPY web/dist /app/web/dist
USER tf
WORKDIR /app
ENV TF_WEB_DIR=/app/web/dist
EXPOSE 8080 4318
CMD ["traceforge"]
