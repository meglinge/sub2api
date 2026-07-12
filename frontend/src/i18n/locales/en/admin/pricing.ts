export default {
  pricing: {
    title: 'Model Pricing',
    description: 'View and manage model pricing data',
    forceUpdate: 'Force Update',
    updating: 'Updating...',
    uploadFile: 'Upload File',
    uploading: 'Uploading...',
    status: {
      title: 'Pricing Service Status',
      description: 'Current pricing data source and update configuration',
      modelCount: 'Model Count',
      lastUpdated: 'Last Updated',
      updateInterval: 'Update Interval',
      hash: 'Data Hash'
    },
    lookup: {
      title: 'Model Price Lookup',
      description: 'Enter a model name to look up the resolved billing price (supports fuzzy matching)',
      placeholder: 'Enter model name, e.g. claude-sonnet-4',
      button: 'Lookup'
    },
    list: {
      title: 'Price List',
      description: '{count} models total',
      searchPlaceholder: 'Search model name or provider...',
      allProviders: 'All Providers',
      showing: 'Showing {count} / {total}',
      model: 'Model',
      provider: 'Provider',
      mode: 'Mode',
      inputCost: 'Input $/MTok',
      outputCost: 'Output $/MTok',
      cacheCreate: 'Cache Create',
      cacheRead: 'Cache Read',
      caching: 'Caching',
      noData: 'No data available'
    }
  }
}
