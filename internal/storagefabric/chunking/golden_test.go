package chunking

import (
	"bytes"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/hashing"
)

// The gear table is part of the wire format, so it is pinned as data.
//
// It is derived in four lines rather than pasted as 256 magic numbers, and a
// derivation is only as pinned as the test that pins it — this is that test. If
// the seed, the digest function or the byte order ever change, this fails
// before two peers can quietly compute different manifests for one blob.
func TestGearTableIsPinned(t *testing.T) {
	got, n, err := hashing.HashReader(bytes.NewReader(gearBytes()))
	if err != nil {
		t.Fatal(err)
	}
	if n != 256*8 {
		t.Fatalf("serialised gear table is %d bytes, want %d", n, 256*8)
	}
	if got.String() != gearDigest {
		t.Fatalf("gear table digest = %s, want %s — the table changed, which is a chunk-format break", got, gearDigest)
	}

	// A table with duplicate or zero entries would still hash to something
	// stable while making boundaries far less content-sensitive.
	seen := make(map[uint64]bool, len(gear))
	for i, v := range gear {
		if v == 0 {
			t.Errorf("gear[%d] is zero", i)
		}
		if seen[v] {
			t.Errorf("gear[%d] = %#x is a duplicate", i, v)
		}
		seen[v] = true
	}
}

type goldenChunk struct {
	offset int64
	length int64
	digest string
}

type goldenCase struct {
	name string
	cfg  Config
	size int
	seed uint64
	// fixture pins the generated input itself. Without it a change to
	// pseudoRandom would silently move every boundary and the only symptom
	// would be a golden table that "needed regenerating".
	fixture string
	chunks  []goldenChunk
}

// Golden chunk boundaries for committed fixtures.
//
// CI asserts this on linux, darwin and windows (the chunking-determinism job in
// .github/workflows/ci.yml). Two peers that cut the same bytes differently
// compute different manifests for one blob, and every reuse and repair decision
// between them is then wrong — but nothing goes red, transfers simply stop
// deduplicating. A determinism test that runs on one operating system proves
// that operating system.
//
// The fixture is generated rather than committed as a testdata file because
// reading a file would mean importing os, and this package's purity is fenced
// by depguard. The generator is eleven lines of arithmetic in helpers_test.go
// and the bytes it produces are pinned by digest below, so it is a committed
// fixture in every sense that matters.
var goldens = []goldenCase{
	{
		name:    "12 MiB of pseudo-random data at the default parameters",
		cfg:     Config{Min: DefaultMin, Avg: DefaultAvg, Max: DefaultMax},
		size:    12 << 20,
		seed:    0xC0FFEE,
		fixture: "blake3:0c8e0416d404a2156d2abb1fd542d80e5448a7233504fd2adea0f688f8ce6f0d",
		chunks: []goldenChunk{
			{0, 383581, "blake3:4b619150de516aad59adfd65e3cb0f19c3e5ee60b868ebad3c56255cc4a770a2"},
			{383581, 398560, "blake3:66b5e11313c043d6ec9c46fd3708c6ed057911fa0e8f3624b3d1417fec0905f3"},
			{782141, 449754, "blake3:bf1323b78287e7358fa8288733a3dc48afe8ddcd7b108a3650cb10b57c31ea9f"},
			{1231895, 1214651, "blake3:783b8d49e5df4f99194848b10acd7fea0ad5c0fbfdaba26c684dcfcc3065d64e"},
			{2446546, 561849, "blake3:66f0adfc5b65252c30be4169233b03d1aed6a113ce01006629cbd7ff5737319a"},
			{3008395, 424739, "blake3:2ff1a221bccd9a25c2ca1db57c3399c89f841063e8a478ce4094c5b13c6768f7"},
			{3433134, 293802, "blake3:cb14935fc30d3343142d27244ce8005033c456ac84fec1a8e77f98088a853185"},
			{3726936, 1218294, "blake3:adcf5b945610d3b587bbe8a1d753874b1d1ce6f6541a66029ce684b08ad08f36"},
			{4945230, 1121609, "blake3:923fb4cff7038c533bc0931a18a7dc7c4f040d5786b192732424e9bfc22c9e15"},
			{6066839, 1085280, "blake3:6c973a12bb36b16d6754e75947fc963a073ec7a7c5b2afd7dd7eaf303141262f"},
			{7152119, 382245, "blake3:17c32681442aaff65dc398944f8c5c21f7b337c2a3c2bd1c111a97ed3aa7e437"},
			{7534364, 1034002, "blake3:dc472884ad361420cc2ddab0ab892ef3781df0d6dbea23f95b7e60bffb522889"},
			{8568366, 1222943, "blake3:b67be7f5f05073e4ce28884d154dba7560e2692bd9244a60d2f256d43af41469"},
			{9791309, 1100614, "blake3:89cfef9ee4c5312169e5fba142dabbae7de07c5a58f5c3c62778e49fa07e6f02"},
			{10891923, 1163070, "blake3:04d2f87fd2108126d793e97de58f93b34eb8659eaebe4ad2710ae1682ce6e23d"},
			{12054993, 527919, "blake3:a819b120df18bcfd81e64072e27e885a5d11a57b668f5a379a5f570a803d0c19"},
		},
	},
	{
		name:    "128 KiB of pseudo-random data at 1/4/16 KiB parameters",
		cfg:     Config{Min: 1 << 10, Avg: 4 << 10, Max: 16 << 10},
		size:    128 << 10,
		seed:    0xC0FFEE,
		fixture: "blake3:b2f813df51c7c445f84ac9a11c1a727225fd16550e9110a1e8ac15b49205cebf",
		chunks: []goldenChunk{
			{0, 8836, "blake3:bc98e702562ed4a06f0ce6346e9fdf10eeb8d11ce078b09b9747ba13f9cefb5f"},
			{8836, 4551, "blake3:a6315568de981ed312f1ca479b3233695dad9e844ac075e59806f9f2774459f1"},
			{13387, 4442, "blake3:feb829276298a7b969f0bdd5b5fab580c02258c3886bb28cdc36a88eb42b3ec1"},
			{17829, 6002, "blake3:94820e4aafd36425740e9bac2968ed161682562caebc41681be5b3985f2e1424"},
			{23831, 5020, "blake3:480e5d0ee242850d6dab02c816a9fc22a82bccaa657dc6e3202ba12edbbee666"},
			{28851, 4388, "blake3:9dd9bedfc43b64b073528329f87a17df5a4610da36404ed478c4b24904f98f48"},
			{33239, 4182, "blake3:2d3259f26dd9950b46df25b112c9e0a20b2f6b387cd3f9538dd590fe9373d2f2"},
			{37421, 2813, "blake3:8bc5203f87d66a71bdc19113cd061c7e5b8ef9eb81ab9c7f970fb7ef69fb2c39"},
			{40234, 5606, "blake3:076e8989e841baa07f9d8c1c744d9ade21cd0d799c57ce6d7c538e7b5a3ec1ad"},
			{45840, 4316, "blake3:dc2a27f03139c4992c15ce3e111d1586364611fc5043fe3c77558096bd3d767b"},
			{50156, 4816, "blake3:510d40538a4d7be73286f3d70a32e38499fe99df51cc2060b3c3baee3927378b"},
			{54972, 2709, "blake3:db910f5a1ae3b55f4480cc527a82c2424ae70328e7c2e253420ee3aefdf1434e"},
			{57681, 1771, "blake3:083ca3c2d59444b359caca0264f84d107dd3acc49061bdbe41a2078a85af35f1"},
			{59452, 4915, "blake3:b471358edb8c6253235f6c0dbd5c38cfcb141b81c74c5001c20631ef025c5c22"},
			{64367, 4984, "blake3:ad0c30c336275efb5d89a749886ccf781e7213f316427a4d9f10576532d8a3ca"},
			{69351, 3140, "blake3:4df8ebfeb9a2da897797112500c66fe4247dc35a3bf764d1d3ecf853254cb1be"},
			{72491, 1101, "blake3:1d3d26f378f4bf8e6df36a8b520cb1cdc7a8b100dd23043397eeab765a293b83"},
			{73592, 4891, "blake3:65646e136fe132d2c834e36a04e40c2ee4086fab171572abb8c39a90aa7b3f33"},
			{78483, 4154, "blake3:5f35e0910e0a813069da57808a5b0f425e45e21633d6b3d462ffc8a17bd4fea8"},
			{82637, 1871, "blake3:25a2a70cb4196d0381bfd67c4c7c47a6f76f1f5bf9b0279a970eda377c632750"},
			{84508, 4421, "blake3:974f542322f5c04951569584a2d0826f290c7121e4a377a9523cd2f4fd754320"},
			{88929, 4062, "blake3:e497a11bbf5a5e51849da142f27b4040649a76c90ccf9f269f7d4ab2bdea4000"},
			{92991, 2340, "blake3:d2a8937db10e58891b6948ea93b9d7ca26807ee6ac71621881f72f0844f33389"},
			{95331, 4355, "blake3:4f767c51f9dc9614c2f41bd35ded20d45152e9eafe26e8802ece2bf6a60cff56"},
			{99686, 10172, "blake3:2376e5b65cfb5ca7c3c91a626899a82f74ccc7beda7c33b9c9670e3f977918de"},
			{109858, 6324, "blake3:bf894ee6b8571ec8bdffd9db758ed0f9db9f5336facc418d2963f7d4c395571e"},
			{116182, 4428, "blake3:4dbcdf81211940dc521521020e5311cc4caabb3833bc0512fd66d8acfcfa03f4"},
			{120610, 2224, "blake3:7f4503a54a364716099aeb794f811ba57582dd617f03d8e42a2d0793a0812620"},
			{122834, 4433, "blake3:dcbf9795f134cedf6a859107de9280908d6e5d6bcc2629f69931233b72ee3020"},
			{127267, 3805, "blake3:f2eeda64f39a3e3894995a87a3bb961d998177890b9bfcd60f3ad56b25cda1a7"},
		},
	},
}

func TestGoldenBoundariesAreStableAcrossPlatforms(t *testing.T) {
	for _, g := range goldens {
		t.Run(g.name, func(t *testing.T) {
			data := pseudoRandom(g.size, g.seed)
			if got := digestOf(t, data); got.String() != g.fixture {
				t.Fatalf("fixture digest = %s, want %s — the input changed, so the golden below is not describing what it claims to", got, g.fixture)
			}

			got := chunkBytes(t, data, g.cfg)
			if len(got) != len(g.chunks) {
				t.Fatalf("produced %d chunks, the golden has %d", len(got), len(g.chunks))
			}
			for i, want := range g.chunks {
				if got[i].Offset != want.offset || got[i].Length != want.length || got[i].Digest.String() != want.digest {
					t.Errorf("chunk %d = {offset %d, length %d, %s}, want {offset %d, length %d, %s}",
						i, got[i].Offset, got[i].Length, got[i].Digest, want.offset, want.length, want.digest)
				}
			}
		})
	}
}
