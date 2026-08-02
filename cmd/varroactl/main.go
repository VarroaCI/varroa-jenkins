package main

import "os"

var version = "dev"

func main() {
	os.Exit(run(os.Args[1:]))
}
