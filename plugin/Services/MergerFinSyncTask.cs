using System;
using System.Collections.Generic;
using System.Threading;
using System.Threading.Tasks;
using MediaBrowser.Model.Tasks;

namespace Jellyfin.Plugin.MergerFin.Services;

/// <summary>
/// Native Jellyfin Scheduled Task that syncs remote Jellyfin catalogs on a 30-minute interval.
/// </summary>
public class MergerFinSyncTask : IScheduledTask
{
    private readonly CatalogSyncService _syncService;

    public MergerFinSyncTask(CatalogSyncService syncService)
    {
        _syncService = syncService;
    }

    public string Name => "Sync MergerFin Remote Servers";

    public string Key => "MergerFinCatalogSync";

    public string Description => "Indexes movies and episodes from configured remote Jellyfin servers and updates remote versions.";

    public string Category => "MergerFin";

    public Task ExecuteAsync(IProgress<double> progress, CancellationToken cancellationToken)
    {
        return _syncService.SyncAllAsync(progress, cancellationToken);
    }

    public IEnumerable<TaskTriggerInfo> GetDefaultTriggers()
    {
        return
        [
            new TaskTriggerInfo
            {
                Type = TaskTriggerInfoType.IntervalTrigger,
                IntervalTicks = TimeSpan.FromMinutes(30).Ticks
            }
        ];
    }
}
