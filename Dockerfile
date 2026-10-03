# 着信制御・Push 送信サービス
FROM golang:1-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/myvoip-server ./cmd/myvoip-server

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/myvoip-server /usr/local/bin/myvoip-server
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/myvoip-server"]
CMD ["serve"]
