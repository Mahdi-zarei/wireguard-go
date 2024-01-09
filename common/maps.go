package common

const DeleteThreshold = 1 << 15

func CleanMap[K comparable, V any](m map[K]V) map[K]V {
	nmap := make(map[K]V)
	for key, val := range m {
		nmap[key] = val
	}
	return nmap
}
