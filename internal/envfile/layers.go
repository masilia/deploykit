package envfile

// Layers is an ordered lookup, most specific first. The legacy cfg_get is Layers{target, global}: the selected client's
// file wins, then the editor's deploy.env.
type Layers []*Doc

// Get returns the first non-empty value of key across the layers (nil layers, such as an absent file, are skipped),
// or def. An empty value in a more specific layer falls through to the next one, as in cfg_get.
func (l Layers) Get(key, def string) string {
	for _, d := range l {
		if d == nil {
			continue
		}
		if v := d.Get(key, ""); v != "" {
			return v
		}
	}
	return def
}
