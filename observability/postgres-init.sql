CREATE ROLE if3110_target LOGIN PASSWORD 'if3110-target-demo';
CREATE DATABASE if3110_target OWNER if3110_target;

\connect if3110_target
SET ROLE if3110_target;
CREATE TABLE public.observability_demo (id bigint PRIMARY KEY, visits integer NOT NULL);
INSERT INTO public.observability_demo (id, visits) SELECT id, 0 FROM generate_series(1, 8) AS id;
RESET ROLE;
