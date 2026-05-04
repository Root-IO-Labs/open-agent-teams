package names

import (
	"math/rand"
	"time"
)

var (
	adjectives = []string{
		"happy", "clever", "brave", "calm", "eager",
		"fancy", "gentle", "jolly", "kind", "lively",
		"nice", "proud", "silly", "witty", "zealous",
		"bright", "swift", "bold", "cool", "wise",
		"mighty", "cosmic", "stellar", "lunar", "solar",
		"crystal", "thunder", "mystic", "noble", "royal",
		"azure", "crimson", "golden", "silver", "jade",
		"frost", "flame", "storm", "ocean", "forest",
		"shadow", "light", "dawn", "dusk", "twilight",
		"quantum", "nexus", "prime", "ultra", "mega",
	}

	animals = []string{
		"platypus", "elephant", "dolphin", "penguin", "koala",
		"otter", "panda", "tiger", "lion", "bear",
		"fox", "wolf", "eagle", "hawk", "owl",
		"deer", "rabbit", "squirrel", "badger", "raccoon",
		"falcon", "raven", "phoenix", "dragon", "kraken",
		"lynx", "panther", "jaguar", "leopard", "cheetah",
		"bison", "moose", "elk", "walrus", "seal",
		"crane", "heron", "pelican", "albatross", "condor",
		"shark", "whale", "manta", "octopus", "seahorse",
		"cobra", "python", "viper", "gecko", "iguana",
	}

	rng *rand.Rand
)

func init() {
	rng = rand.New(rand.NewSource(time.Now().UnixNano()))
}

// Generate creates a Docker-style name (adjective-animal)
func Generate() string {
	adj := adjectives[rng.Intn(len(adjectives))]
	animal := animals[rng.Intn(len(animals))]
	return adj + "-" + animal
}
