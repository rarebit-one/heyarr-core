## heyarr config print

Print the fully resolved configuration

### Synopsis

Print configuration after defaults, the config file and HEYARR_ environment
have been layered and validated — which is what Heyarr will actually use, and
frequently not what any single source says.

```
heyarr config print [flags]
```

### Options

```
      --redacted   hide secret values (no-op today — configuration holds no secrets)
```

### Options inherited from parent commands

```
  -c, --config string   path to the configuration file (default: built-in defaults plus HEYARR_ environment)
```

### See also

* [heyarr config](heyarr_config.md)	 - Inspect configuration
