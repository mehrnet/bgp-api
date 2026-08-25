CREATE TABLE public.bgp_api_dataset (
  singleton BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (singleton),
  release_tag TEXT NOT NULL,
  dataset_schema TEXT NOT NULL,
  built_at TEXT,
  source_commit TEXT,
  activated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE VIEW public.lookup_prefixes AS
  SELECT source, prefix_key, prefix_length, start_ip_sort, end_ip_sort, ip_version,
         registry, country, netname, cidr, asn, region, city, status,
         allocation_date, created, last_modified, record_source, mnt_by, org,
         abuse_contact, description
  FROM :"dataset_schema".lookup_prefixes;
CREATE VIEW public.allocation_objects AS
  SELECT id, start_ip_sort, end_ip_sort, ip_version, registry, country, netname,
         status, allocation_date, created, last_modified, record_source, mnt_by,
         org, abuse_contact, description
  FROM :"dataset_schema".allocation_objects;
CREATE VIEW public.route_objects AS
  SELECT id, prefix, prefix_length, start_ip_sort, end_ip_sort, ip_version,
         origin_asn, asn_number, registry, record_source, mnt_by, org,
         abuse_contact, description
  FROM :"dataset_schema".route_objects;
CREATE VIEW public.autnums AS
  SELECT id, asn, asn_number, registry, country, as_name, org, status, created,
         last_modified, record_source, mnt_by, abuse_contact, description
  FROM :"dataset_schema".autnums;
CREATE VIEW public.range_summaries AS
  SELECT cidr, ip_version, prefix_length, allocation_records, route_records,
         countries, asns
  FROM :"dataset_schema".range_summaries;

INSERT INTO public.bgp_api_dataset (singleton, release_tag, dataset_schema, built_at, source_commit)
SELECT TRUE, release_tag, :'dataset_schema', built_at, source_commit
FROM :"dataset_schema".dataset_metadata
LIMIT 1;

GRANT USAGE ON SCHEMA public TO bgp_api;
GRANT SELECT ON public.bgp_api_dataset, public.lookup_prefixes, public.allocation_objects,
  public.route_objects, public.autnums, public.range_summaries TO bgp_api;
