export default {
  pricing: {
    title: '价目表管理',
    description: '查看和管理模型计费价格',
    forceUpdate: '强制更新',
    updating: '更新中...',
    uploadFile: '上传文件',
    uploading: '上传中...',
    status: {
      title: '价格服务状态',
      description: '当前价格数据源和更新配置',
      modelCount: '模型数量',
      lastUpdated: '最后更新',
      updateInterval: '更新间隔',
      hash: '数据哈希'
    },
    lookup: {
      title: '模型价格查询',
      description: '输入模型名称查询实际匹配的计费价格（支持模糊匹配）',
      placeholder: '输入模型名称，如 claude-sonnet-4',
      button: '查询'
    },
    list: {
      title: '价格列表',
      description: '共 {count} 个模型',
      searchPlaceholder: '搜索模型名称或提供商...',
      allProviders: '所有提供商',
      showing: '显示 {count} / {total}',
      model: '模型',
      provider: '提供商',
      mode: '类型',
      inputCost: '输入 $/MTok',
      outputCost: '输出 $/MTok',
      cacheCreate: '缓存创建',
      cacheRead: '缓存读取',
      caching: '缓存',
      noData: '暂无数据'
    }
  }
}
