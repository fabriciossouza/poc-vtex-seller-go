FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ENV CGO_ENABLED=0
RUN go build -trimpath -ldflags="-s -w" -o /out/seller ./cmd/seller \
 && go build -trimpath -ldflags="-s -w" -o /out/tms-mock ./cmd/tms-mock

FROM gcr.io/distroless/static-debian12:nonroot AS seller
COPY --from=build /out/seller /seller
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/seller"]

FROM gcr.io/distroless/static-debian12:nonroot AS tms-mock
COPY --from=build /out/tms-mock /tms-mock
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/tms-mock"]
