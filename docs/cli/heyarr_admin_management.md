## heyarr admin management

Authorise enrolled devices for write (ADR-0065), without the HTTP API

### Synopsis

Authorise an enrolled device for write: with a grant, the device's own
credential carries write as well as read (ADR-0065). This is the same record
as POST /api/v1/session/management-grants, written on the host — the Mnemosyne
personal profile does not mount that route.

A grant names a device KEY, not a user: grant only a device you control, and
revoke it when the device is retired. Grants are local to this controller.

### Options inherited from parent commands

```
  -c, --config string   path to the configuration file (default: $HEYARR_CONFIG, else /etc/heyarr/config.yaml if present, else built-in defaults plus HEYARR_ environment)
```

### See also

* [heyarr admin](heyarr_admin.md)	 - Host administration of the controller database
* [heyarr admin management grant](heyarr_admin_management_grant.md)	 - Authorise a device for write
* [heyarr admin management list](heyarr_admin_management_list.md)	 - List the devices authorised for write
* [heyarr admin management revoke](heyarr_admin_management_revoke.md)	 - Return a device to the read floor
