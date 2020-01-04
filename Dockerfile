#FROM golang:alpine3.11
#LABEL app="pod ip reporter"
#WORKDIR /app
#COPY . .
#RUN go build -o main .
#EXPOSE 5700
#CMD ["./main"]

FROM golang:1.13
WORKDIR /app
COPY . .
#RUN go build -ldflags "-linkmode external -extldflags -static" -a main
RUN GOOS=linux GOARCH=amd64 go build -ldflags "-c -w -s -linkmode external -extldflags -static" -a main

FROM scratch
COPY --from=0 /app /app
ENTRYPOINT ["/app/main"]
