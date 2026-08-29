# Projects & DSNs

A project is one DSN. Any SDK holding that DSN reports into it — the
`platform` argument is a label, not a filter.

Use one project per platform (`shop-ios`, `shop-android`): issues never merge
across platforms, but the overview's "latest release" and the crash-spike
baseline are computed per project, so mixing platforms in one project blends
those two numbers.

```sh
docker compose exec crashcart /crashcart project shop-ios     "Shop app (iOS)"     ios
docker compose exec crashcart /crashcart project shop-android "Shop app (Android)" android
```
