## 0.0.5 / 2024-01-15

* [FEATURE] add flag to filter foreman hosts search

## 0.0.4 / 2024-01-12

* [FEATURE] add user agent http header in foreman requests
* [FEATURE] add flag to lock concurrent requests on collectors


## 0.0.3 / 2023-12-15

* [ENHANCEMENT] logging messages more clear about skipping metrics collection
* [ENHANCEMENT] hostfact collector: use jsonCodec encode func and add dedicated updateKV func
* [FIX] enable hostfact collector caching if flag `--cache.enabled` is given


## 0.0.2 / 2023-12-14

* [FEATURE] add skip-tls-verify flag for foreman HTTP client
* [FIX] name label in hostfact collector (consistency with host collector)

## 0.0.1 / 2023-12-13

* [FEATURE] first version
