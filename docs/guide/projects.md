# Projects & DSNs

A **project** is one DSN. Any SDK holding that DSN reports into it.

## Creating projects

From the CLI:

```sh
crashcart project <slug> <name> [platform]
crashcart project shop-ios "Shop app (iOS)" ios
```

From the [API](/reference/api#projects):

```sh
curl -H "Authorization: Bearer $API_KEY" -H "Content-Type: application/json" \
     -d '{"slug":"shop-ios","name":"Shop app (iOS)","platform":"ios"}' \
     http://localhost:8080/api/projects
```

Or from the viewer's home page.

The **slug** is the stable identifier used in viewer URLs (`/p/shop-ios/…`)
and API paths (`/api/projects/shop-ios/…`), and in the
[export format](/reference/export-format), which refers to projects by
slug so a dump loads into any database. The numeric **id** appears only in
the DSN.

## The DSN

```
http://<public_key>@crashcart.example.com/<project_id>
```

The key authenticates ingest. Anyone holding it can send events to the
project — treat it like any client-side credential: fine to ship in an app
binary, not something to publish in a README.

Set [`PUBLIC_URL`](/deploy/configuration) so the DSN printed by the CLI and
shown in the viewer uses the address your apps can actually reach.

### Rotating the key

```sh
crashcart rotate-key shop-ios
# project shop-ios: new DSN http://<new-key>@…/1
```

or **Settings → Rotate key** in the viewer. The old key stops working
immediately; apps in the field with the old DSN will be rejected until they
ship a build with the new one.

## One project per platform

Use one project per app *and* platform (`shop-ios`, `shop-android`,
`shop-web`) rather than one project for the whole app. The `platform`
argument is a label, not a filter:

- Issues never merge across platforms anyway — an iOS stack and an Android
  stack fingerprint differently.
- But the overview's **latest release** and the **crash-spike baseline**
  are computed per project. iOS `2.4.1` and Android `2.4.1` are different
  builds with different crash rates; mixing them in one project blends those
  two numbers.

## Sampling and quota

Each project has three knobs, editable under **Settings → Sampling** or via
`PATCH /api/projects/{slug}`:

| Field | Default | Meaning |
|---|---|---|
| `sample_keep_first` | `100` | The first *N* events of every issue are always stored |
| `sample_rate` | `1.0` | After that, this fraction of each issue's events is stored |
| `daily_quota` | `100000` | Events accepted per UTC day; further envelopes are dropped. `0` = unlimited |

Sampling is **per issue** and **counts stay exact**: a dropped event still
increments the issue's `event_count`, so the numbers you triage by are
right, while `stored_count` and storage are bounded. `fatal` events are
always stored.

See [Issues & grouping](./issues#sampling) for how this interacts with
grouping.
