package chunking

type Chunker interface {
	Split(string) []string
}
