# Build Stage
FROM golang:1.26-trixie AS build-stage

RUN apt-get update && apt-get install -y \
        ca-certificates

WORKDIR /app

COPY Makefile go.mod go.sum ./
RUN make setup

COPY . ./
RUN make

# Final Stage
FROM debian:trixie-slim

RUN apt-get update && apt-get install -y \
        ca-certificates

WORKDIR /app

COPY --from=build-stage /app/kostal2influx  /app/
RUN chmod +x /app/

CMD ["/app/kostal2influx"]
