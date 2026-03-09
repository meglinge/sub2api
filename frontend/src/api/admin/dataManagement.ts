import { apiClient } from '../client'

export type BackupType = 'postgres' | 'redis' | 'full'
export type BackupJobStatus = 'queued' | 'running' | 'succeeded' | 'failed' | 'partial_succeeded'

export interface BackupAgentInfo {
  status: string
  version: string
  uptime_seconds: number
}

export interface BackupAgentHealth {
  enabled: boolean
  reason: string
  socket_path: string
  agent?: BackupAgentInfo
}

export interface DataManagementPostgresConfig {
  host: string
  port: number
  user: string
  password?: string
  password_configured?: boolean
  database: string
  ssl_mode: string
  container_name: string
}

export interface DataManagementRedisConfig {
  addr: string
  username: string
  password?: string
  password_configured?: boolean
  db: number
  container_name: string
}

export interface DataManagementS3Config {
  enabled: boolean
  endpoint: string
  region: string
  bucket: string
  access_key_id: string
  secret_access_key?: string
  secret_access_key_configured?: boolean
  prefix: string
  force_path_style: boolean
  use_ssl: boolean
}

export interface DataManagementConfig {
  source_mode: 'direct' | 'docker_exec'
  backup_root: string
  sqlite_path?: string
  retention_days: number
  keep_last: number
  active_postgres_profile_id?: string
  active_redis_profile_id?: string
  active_s3_profile_id?: string
  postgres: DataManagementPostgresConfig
  redis: DataManagementRedisConfig
  s3: DataManagementS3Config
}

export type SourceType = 'postgres' | 'redis'

export interface DataManagementSourceConfig {
  host: string
  port: number
  user: string
  password?: string
  database: string
  ssl_mode: string
  addr: string
  username: string
  db: number
  container_name: string
}

export interface DataManagementSourceProfile {
  source_type: SourceType
  profile_id: string
  name: string
  is_active: boolean
  password_configured?: boolean
  config: DataManagementSourceConfig
  created_at?: string
  updated_at?: string
}

export interface TestS3Request {
  endpoint: string
  region: string
  bucket: string
  access_key_id: string
  secret_access_key: string
  prefix?: string
  force_path_style?: boolean
  use_ssl?: boolean
}

export interface TestS3Response {
  ok: boolean
  message: string
}

export interface CreateBackupJobRequest {
  backup_type: BackupType
  upload_to_s3?: boolean
  s3_profile_id?: string
  postgres_profile_id?: string
  redis_profile_id?: string
  idempotency_key?: string
}

export interface CreateBackupJobResponse {
  job_id: string
  status: BackupJobStatus
}

export interface BackupArtifactInfo {
  local_path: string
  size_bytes: number
  sha256: string
}

export interface BackupS3Info {
  bucket: string
  key: string
  etag: string
}

export interface BackupJob {
  job_id: string
  backup_type: BackupType
  status: BackupJobStatus
  triggered_by: string
  s3_profile_id?: string
  postgres_profile_id?: string
  redis_profile_id?: string
  started_at?: string
  finished_at?: string
  error_message?: string
  artifact?: BackupArtifactInfo
  s3?: BackupS3Info
}

export interface ListSourceProfilesResponse {
  items: DataManagementSourceProfile[]
}

export interface CreateSourceProfileRequest {
  profile_id: string
  name: string
  config: DataManagementSourceConfig
  set_active?: boolean
}

export interface UpdateSourceProfileRequest {
  name: string
  config: DataManagementSourceConfig
}

export interface DataManagementS3Profile {
  profile_id: string
  name: string
  is_active: boolean
  s3: DataManagementS3Config
  secret_access_key_configured?: boolean
  created_at?: string
  updated_at?: string
}

export interface ListS3ProfilesResponse {
  items: DataManagementS3Profile[]
}

export interface CreateS3ProfileRequest {
  profile_id: string
  name: string
  enabled: boolean
  endpoint: string
  region: string
  bucket: string
  access_key_id: string
  secret_access_key?: string
  prefix?: string
  force_path_style?: boolean
  use_ssl?: boolean
  set_active?: boolean
}

export interface UpdateS3ProfileRequest {
  name: string
  enabled: boolean
  endpoint: string
  region: string
  bucket: string
  access_key_id: string
  secret_access_key?: string
  prefix?: string
  force_path_style?: boolean
  use_ssl?: boolean
}

export interface ListBackupJobsRequest {
  page_size?: number
  page_token?: string
  status?: BackupJobStatus
  backup_type?: BackupType
}

export interface ListBackupJobsResponse {
  items: BackupJob[]
  next_page_token?: string
}

export async function getAgentHealth(): Promise<BackupAgentHealth> {
  const { data } = await apiClient.get<BackupAgentHealth>('/admin/data-management/agent/health')
  return data
}

export async function getConfig(): Promise<DataManagementConfig> {
  const { data } = await apiClient.get<DataManagementConfig>('/admin/data-management/config')
  return data
}

export async function updateConfig(request: DataManagementConfig): Promise<DataManagementConfig> {
  const { data } = await apiClient.put<DataManagementConfig>('/admin/data-management/config', request)
  return data
}

export async function testS3(request: TestS3Request): Promise<TestS3Response> {
  const { data } = await apiClient.post<TestS3Response>('/admin/data-management/s3/test', request)
  return data
}

export async function listSourceProfiles(sourceType: SourceType): Promise<ListSourceProfilesResponse> {
  const { data } = await apiClient.get<ListSourceProfilesResponse>(`/admin/data-management/sources/${sourceType}/profiles`)
  return data
}

export async function createSourceProfile(sourceType: SourceType, request: CreateSourceProfileRequest): Promise<DataManagementSourceProfile> {
  const { data } = await apiClient.post<DataManagementSourceProfile>(`/admin/data-management/sources/${sourceType}/profiles`, request)
  return data
}

export async function updateSourceProfile(sourceType: SourceType, profileID: string, request: UpdateSourceProfileRequest): Promise<DataManagementSourceProfile> {
  const { data } = await apiClient.put<DataManagementSourceProfile>(`/admin/data-management/sources/${sourceType}/profiles/${profileID}`, request)
  return data
}

export async function deleteSourceProfile(sourceType: SourceType, profileID: string): Promise<void> {
  await apiClient.delete(`/admin/data-management/sources/${sourceType}/profiles/${profileID}`)
}

export async function setActiveSourceProfile(sourceType: SourceType, profileID: string): Promise<DataManagementSourceProfile> {
  const { data } = await apiClient.post<DataManagementSourceProfile>(`/admin/data-management/sources/${sourceType}/profiles/${profileID}/activate`)
  return data
}

export async function listS3Profiles(): Promise<ListS3ProfilesResponse> {
  const { data } = await apiClient.get<ListS3ProfilesResponse>('/admin/data-management/s3/profiles')
  return data
}

export async function createS3Profile(request: CreateS3ProfileRequest): Promise<DataManagementS3Profile> {
  const { data } = await apiClient.post<DataManagementS3Profile>('/admin/data-management/s3/profiles', request)
  return data
}

export async function updateS3Profile(profileID: string, request: UpdateS3ProfileRequest): Promise<DataManagementS3Profile> {
  const { data } = await apiClient.put<DataManagementS3Profile>(`/admin/data-management/s3/profiles/${profileID}`, request)
  return data
}

export async function deleteS3Profile(profileID: string): Promise<void> {
  await apiClient.delete(`/admin/data-management/s3/profiles/${profileID}`)
}

export async function setActiveS3Profile(profileID: string): Promise<DataManagementS3Profile> {
  const { data } = await apiClient.post<DataManagementS3Profile>(`/admin/data-management/s3/profiles/${profileID}/activate`)
  return data
}

export async function createBackupJob(request: CreateBackupJobRequest): Promise<CreateBackupJobResponse> {
  const headers = request.idempotency_key
    ? { 'X-Idempotency-Key': request.idempotency_key }
    : undefined

  const { data } = await apiClient.post<CreateBackupJobResponse>(
    '/admin/data-management/backups',
    request,
    { headers }
  )
  return data
}

export async function listBackupJobs(request?: ListBackupJobsRequest): Promise<ListBackupJobsResponse> {
  const { data } = await apiClient.get<ListBackupJobsResponse>('/admin/data-management/backups', {
    params: request
  })
  return data
}

export async function getBackupJob(jobID: string): Promise<BackupJob> {
  const { data } = await apiClient.get<BackupJob>(`/admin/data-management/backups/${jobID}`)
  return data
}

export interface PostgresInfo {
  host: string
  port: number
  dbname: string
  user: string
  sslmode: string
  tools_ok: boolean
  tools_error: string
}

export interface PostgresRestoreResult {
  restored: boolean
  need_restart: boolean
  message: string
}

export interface ChunkedUploadInitResult {
  upload_id: string
  chunk_size: number
  chunk_count: number
}

export interface ChunkedUploadChunkResult {
  upload_id: string
  index: number
  received: number
  total: number
  complete: boolean
}

export async function getPostgresInfo(): Promise<PostgresInfo> {
  const { data } = await apiClient.get<PostgresInfo>('/admin/data-management/postgres/info')
  return data
}

export function getPostgresExportUrl(): string {
  return `${apiClient.defaults.baseURL}/admin/data-management/postgres/export`
}

export async function restorePostgresDatabase(file: File, confirm: string): Promise<PostgresRestoreResult> {
  const formData = new FormData()
  formData.append('file', file)
  formData.append('confirm', confirm)
  const { data } = await apiClient.post<PostgresRestoreResult>(
    '/admin/data-management/postgres/restore',
    formData,
    {
      headers: { 'Content-Type': 'multipart/form-data' },
      timeout: 600000,
    }
  )
  return data
}

export async function initChunkedRestore(filename: string, totalSize: number): Promise<ChunkedUploadInitResult> {
  const { data } = await apiClient.post<ChunkedUploadInitResult>(
    '/admin/data-management/postgres/restore/init',
    { filename, total_size: totalSize }
  )
  return data
}

export async function uploadRestoreChunk(uploadId: string, index: number, chunk: Blob): Promise<ChunkedUploadChunkResult> {
  const formData = new FormData()
  formData.append('chunk', chunk)
  formData.append('index', String(index))
  const { data } = await apiClient.post<ChunkedUploadChunkResult>(
    `/admin/data-management/postgres/restore/upload/${uploadId}`,
    formData,
    {
      headers: { 'Content-Type': 'multipart/form-data' },
      timeout: 120000,
    }
  )
  return data
}

export async function completeChunkedRestore(uploadId: string, confirm: string): Promise<PostgresRestoreResult> {
  const { data } = await apiClient.post<PostgresRestoreResult>(
    `/admin/data-management/postgres/restore/complete/${uploadId}`,
    { confirm },
    { timeout: 600000 }
  )
  return data
}

export async function abortChunkedRestore(uploadId: string): Promise<void> {
  await apiClient.delete(`/admin/data-management/postgres/restore/upload/${uploadId}`)
}

export const dataManagementAPI = {
  getAgentHealth,
  getConfig,
  updateConfig,
  listSourceProfiles,
  createSourceProfile,
  updateSourceProfile,
  deleteSourceProfile,
  setActiveSourceProfile,
  testS3,
  listS3Profiles,
  createS3Profile,
  updateS3Profile,
  deleteS3Profile,
  setActiveS3Profile,
  createBackupJob,
  listBackupJobs,
  getBackupJob,
  getPostgresInfo,
  getPostgresExportUrl,
  restorePostgresDatabase,
  initChunkedRestore,
  uploadRestoreChunk,
  completeChunkedRestore,
  abortChunkedRestore,
}

export default dataManagementAPI
