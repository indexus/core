package tree

import "testing"

func TestRandomNameNearPreservesRequestedPrefixAtBounds(t *testing.T) {
	for _, target := range [][]byte{nil, {0xa5}, {0xa5, 0x5a}, make([]byte, 32)} {
		for _, keep := range []int{-1, 0, 1, 7, 8, 9, 95, 96, 128, int(^uint(0) >> 1)} {
			name, err := BASE64.RandomNameNear(target, keep)
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := BASE64.Decode(name)
			if err != nil {
				t.Fatal(err)
			}
			want := min(max(keep, 0), len(target)*8, len(decoded)*8)
			for bit := 0; bit < want; bit++ {
				mask := byte(1 << (7 - uint(bit%8)))
				if target[bit/8]&mask != decoded[bit/8]&mask {
					t.Fatalf("prefix changed: target=%x keep=%d bit=%d", target, keep, bit)
				}
			}
		}
	}
}
