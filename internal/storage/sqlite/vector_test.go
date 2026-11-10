package sqlite

import "testing"

func TestVectorEncodingRoundTrip(t *testing.T) {
	vector := []float32{3.25}
	if actual := decodeVector(encodeVector(vector)); len(actual) != 1 || actual[0] != 3.25 {
		t.Fatalf("vector round trip = %#v", actual)
	}
}
