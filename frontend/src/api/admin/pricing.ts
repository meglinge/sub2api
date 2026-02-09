/**
 * Pricing management API endpoints for admin operations
 */

import { apiClient } from '../client'

export interface ModelPricingItem {
  model: string
  input_cost_per_token: number
  output_cost_per_token: number
  input_cost_per_mtok: number
  output_cost_per_mtok: number
  cache_creation_input_token_cost?: number
  cache_read_input_token_cost?: number
  provider: string
  mode: string
  supports_prompt_caching: boolean
  output_cost_per_image?: number
}

export interface PricingListResponse {
  items: ModelPricingItem[]
  total: number
  providers: string[]
}

export interface PricingStatusResponse {
  status: {
    model_count: number
    last_updated: string
    local_hash: string
  }
  config: {
    remote_url: string
    hash_url: string
    data_dir: string
    update_interval_hours: number
    hash_check_interval_minutes: number
  }
}

export interface PricingUpdateResponse {
  message: string
  status: {
    model_count: number
    last_updated: string
    local_hash: string
  }
}

export interface ModelLookupResponse {
  model: string
  pricing: {
    input_cost_per_token: number
    output_cost_per_token: number
    input_cost_per_mtok: number
    output_cost_per_mtok: number
    cache_creation_input_token_cost: number
    cache_read_input_token_cost: number
  }
}

export async function listPricing(params?: { search?: string; provider?: string }): Promise<PricingListResponse> {
  const { data } = await apiClient.get<PricingListResponse>('/admin/pricing', { params })
  return data
}

export async function getPricingStatus(): Promise<PricingStatusResponse> {
  const { data } = await apiClient.get<PricingStatusResponse>('/admin/pricing/status')
  return data
}

export async function forceUpdatePricing(): Promise<PricingUpdateResponse> {
  const { data } = await apiClient.post<PricingUpdateResponse>('/admin/pricing/update')
  return data
}

export async function lookupModel(model: string): Promise<ModelLookupResponse> {
  const { data } = await apiClient.get<ModelLookupResponse>('/admin/pricing/lookup', { params: { model } })
  return data
}

export const pricingAPI = {
  list: listPricing,
  getStatus: getPricingStatus,
  forceUpdate: forceUpdatePricing,
  lookupModel
}

export default pricingAPI
