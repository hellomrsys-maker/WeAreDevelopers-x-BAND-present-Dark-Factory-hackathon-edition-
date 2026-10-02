# Stage 2 Run Guide

## Build

```sh
docker build -t tablekeeper:stage-2 .
```

## Run

```sh
docker run -d --rm -p 8080:8080 -e PORT=8080 tablekeeper:stage-2
```

## Health Check

```sh
curl http://localhost:8080/health
```
