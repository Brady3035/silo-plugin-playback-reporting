# Playback Reporting for Silo

An admin page with playback statistics, in the spirit of Emby's Playback Reporting plugin. It reads Silo's own playback history (`GET /api/v2/admin/playback-history`), so it keeps no database and covers plays from before it was installed.

## Install and auto-update

1. In Silo's admin area, open **Plugins**, select **Add** in the **Repositories** panel, and enter:

   ```
   https://raw.githubusercontent.com/Brady3035/silo-plugin-playback-reporting/main/repository.json
   ```

2. Install **Playback Reporting** from that repository. Its update policy defaults to **Update automatically**, so Silo installs each new release on its next update check.
3. Follow the setup below (API key) in the plugin's settings.

## Tabs

- **Activity**: a bar graph of watch time, plays or smart plays per day, week or month (picked from the date range). Stack by user, media type or stream type; **Separate profiles** splits each account into its profiles. Filter to Movies, TV shows or Audiobooks. Includes a weekday × hour heatmap. Click a user (or profile) in the list under the chart to open them on the Users tab over the same dates.
- **Users**: pick a user (and optionally one profile). Totals for movies, TV and audiobooks, a donut of Direct Play / Remux / Transcode, a donut of what they watch, and a paged list of every session with CSV export. **All time** spans that user's own first to last play.
- **Breakdown**: shares by user, stream type and (with *Track clients* on) client, then top Movies, TV shows and Audiobooks, viewed by Plays, Time or Smart plays, as a ranked list or a pie chart of the top 5, 10, 20 or 50 (the rest grouped as Other). **Arrange tiles** lets you drag the tiles (or use the arrows) into the order you want; **Save order** saves it for everyone who opens the report.
- **History**: every play in a range, with CSV export.

The session tables on **Users** and **History** filter by column: click a value in a row (a user, a type, a stream, Done) to show only rows with it, or open the funnel in a column header to tick values, type a title, or set a minimum watch time. Active filters show as chips above the table, and **Export CSV** exports what's shown.

**Smart plays** = watch time ÷ length of the movie, episode or book. Two half-watched episodes count as one episode watched; a movie watched twice counts 2×.

## Connection check

When the plugin starts it checks, in order: that it can find and reach Silo, that Silo accepts the API key, that the key belongs to an admin, that it can read playback history, and (with Track clients on) that it can read live sessions. The result shows at the top right of the report page (**Connected** or **Connection problem**); a problem also shows a banner naming the failing step and what to do. Select it for the full list and **Check again**. Failures are also written to Silo's plugin log.

## Client tracking storage

With **Track clients** on, each play's client, local/remote and play method are kept in the plugin's storage in Silo (no database of its own). Sessions are grouped by the month they were first seen and gzipped into a fixed pool of 240 storage slots, so it holds about 2 million sessions (about 29 bytes each). Every 5 minutes only the months that changed are rewritten, into free slots before the index moves to them, so a crash can't leave a half-written month. If the pool fills up, the oldest months are dropped first. Notes saved by older versions are converted on first start.

## Customize

Select **Customize** at the top right of the report page:

- **Show audiobooks**: off by default. When on, audiobooks appear in the filters, the Users tab and Breakdown; when off, audiobook plays are left out of every report.
- **Colors** for Direct Play, Remux, Transcode, Movies, TV shows and Audiobooks, and the list of colors used for users and pie slices (add, change or remove up to 16).

Changes preview live and are saved by the plugin for everyone who opens the report.

## Settings (plugin settings page)

| Section | Setting | Effect |
|---|---|---|
| Silo connection | Admin API key | Required. An unscoped key on an admin account (no narrower scope covers admin playback history yet). |
| | Server address | Optional; leave empty to use Silo's internal address. |
| Report filters | Hide plays shorter than (seconds) | Leaves short plays out of every report. |
| | Ignore list | One per line: `username` hides an account, `username/profile` hides one profile. Case-insensitive. |
| Optional reports | Track clients | Polls live sessions every 15 s to record client app, local/remote and the actual play method, for the Clients tile on Breakdown and the History filters. Off by default. |

Filters and the ignore list only hide plays in the reports. Silo's history is never changed. The page's status line shows how many plays are hidden.

## Audiobooks

Turn on **Show audiobooks** under Customize. Audiobooks appear when they're played in Silo's own apps (media type `audiobook`). Listening through Audiobookshelf apps goes through Silo's ABS-compatible API and is not written to the admin playback history, so it doesn't show here.

## Releasing

Releases are built by GitHub Actions; nothing is built by hand.

- **Actions → Release → Run workflow** bumps the latest tag (patch, minor or major), or push a tag yourself: `git tag v0.3.0 && git push origin v0.3.0`.
- The workflow tests, builds `plugin-linux-amd64`, `plugin-linux-arm64` and `plugin-darwin-arm64`, attaches them with `checksums.txt` to a GitHub release, and commits the new `repository.json` to `main`. Silo servers that added the repository pick it up on their next update check.

## Performance

The page loads every play once and computes the reports in the browser, because Silo's access to a plugin page lasts five minutes and isn't renewed. Data is sent gzipped (about 17 bytes per play), so 100,000 plays is about 1.8 MB and 500,000 about 8.5 MB. Measured in Chromium with synthetic data: with 100,000 plays every control responds in about 0.2 s; with 500,000 plays the all-time Activity chart takes about 0.5 s per change and the tab uses about 250 MB of memory.

## Build locally

Requires Go 1.26.

```sh
make check                  # gofmt, vet, tests
make dist VERSION=0.3.0     # release/plugin-<os>-<arch> + checksums.txt
make index VERSION=0.3.0    # repository.json for those binaries
```
