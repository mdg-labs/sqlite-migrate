// Command sqlite-migrate is the CLI entrypoint. It is a thin binary that
// wires the internal/ generation tooling (schemadiff, rename, rebuild,
// sqldefwrap) together with the public sqlitemigrate runtime; it holds no
// generation or runtime logic of its own.
package main

func main() {}
