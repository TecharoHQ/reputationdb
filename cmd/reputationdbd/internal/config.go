package internal

type Config struct {
	GitHubToken      string `json:"github_token"`
	TigrisBucket     string `json:"tigris_bucket"`
	DatabaseCacheDir string `json:"database_cache_dir"`
}
