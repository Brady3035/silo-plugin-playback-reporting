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

- **Activity**: a bar graph of watch time, plays or smart plays per day, week or month (picked from the date range). Stack by user, media type or stream type; **Separate profiles** splits each account into its profiles. Filter to Movies, TV shows or Audiobooks. Includes a weekday × hour heatmap.
- **Users**: pick a user (and optionally one profile). Totals for movies, TV and audiobooks, a donut of Direct Play / Remux / Transcode, a donut of what they watch, and a paged list of every session with CSV export. **All time** spans that user's own first to last play.
- **Breakdown**: top Movies, TV shows and Audiobooks, viewed by Plays, Time or Smart plays.
- **History**: every play in a range, with CSV export.
- **Clients**: only when *Track clients* is on.

**Smart plays** = watch time ÷ length of the movie, episode or book. Two half-watched episodes count as one episode watched; a movie watched twice counts 2×.

## Settings (plugin settings page)

| Section | Setting | Effect |
|---|---|---|
| Silo connection | Admin API key | Required. An unscoped key on an admin account (no narrower scope covers admin playback history yet). |
| | Server address | Optional; leave empty to use Silo's internal address. |
| Report filters | Hide plays shorter than (seconds) | Leaves short plays out of every report. |
| | Ignore list | One per line: `username` hides an account, `username/profile` hides one profile. Case-insensitive. |
| Chart colors | Direct Play, Remux, Transcode, Movies, TV shows, Audiobooks | `#rrggbb`. Empty or invalid values use the defaults. |
| | User colors | Comma-separated `#rrggbb` list for users in the Activity chart (up to 16). |
| Optional reports | Track clients | Polls live sessions every 15 s to record client app and local/remote. Off by default. |

Filters and the ignore list only hide plays in the reports. Silo's history is never changed. The page's status line shows how many plays are hidden.

## Audiobooks

Audiobooks appear when they're played in Silo's own apps (media type `audiobook`). Listening through Audiobookshelf apps goes through Silo's ABS-compatible API and is not written to the admin playback history, so it doesn't show here.

## Releasing

Releases are built by GitHub Actions; nothing is built by hand.

- **Actions → Release → Run workflow** bumps the latest tag (patch, minor or major), or push a tag yourself: `git tag v0.3.0 && git push origin v0.3.0`.
- The workflow tests, builds `plugin-linux-amd64`, `plugin-linux-arm64` and `plugin-darwin-arm64`, attaches them with `checksums.txt` to a GitHub release, and commits the new `repository.json` to `main`. Silo servers that added the repository pick it up on their next update check.

## Build locally

Requires Go 1.26.

```sh
make check                  # gofmt, vet, tests
make dist VERSION=0.3.0     # release/plugin-<os>-<arch> + checksums.txt
make index VERSION=0.3.0    # repository.json for those binaries
```
