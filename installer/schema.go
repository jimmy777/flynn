package installer

func (i *Installer) migrateDB() error {
	sqlStr := `
	CREATE TABLE IF NOT EXISTS credentials (
		id TEXT NOT NULL PRIMARY KEY,
		secret TEXT NOT NULL
	);

  CREATE TABLE IF NOT EXISTS aws_clusters (
    cluster TEXT NOT NULL PRIMARY KEY,
    region TEXT NOT NULL,
    num_instances INTEGER NOT NULL,
    instance_type TEXT NOT NULL,
		vpc_cidr TEXT NOT NULL,
		subnet_cidr TEXT NOT NULL,
		stack_name TEXT NOT NULL,
		stack_id TEXT NOT NULL DEFAULT '',
		dns_zone_id TEXT NOT NULL DEFAULT ''
  );

  CREATE TABLE IF NOT EXISTS instances (
		ip TEXT NOT NULL,
		cluster TEXT NOT NULL,

    FOREIGN KEY(cluster) REFERENCES clusters(id)
	);

	CREATE TABLE IF NOT EXISTS domains (
		name TEXT NOT NULL PRIMARY KEY,
		token TEXT NOT NULL,
		cluster TEXT NOT NULL,

    FOREIGN KEY(cluster) REFERENCES clusters(id)
	);

  CREATE TABLE IF NOT EXISTS clusters (
    id TEXT NOT NULL PRIMARY KEY,
    state TEXT NOT NULL,
		aws_cluster TEXT,
		credential TEXT NOT NULL,

		image TEXT NOT NULL DEFAULT '',
		discovery_token TEXT NOT NULL DEFAULT '',
		controller_key TEXT NOT NULL DEFAULT '',
		controller_pin TEXT NOT NULL DEFAULT '',
		dashboard_login_token TEXT NOT NULL DEFAULT '',
		cert TEXT NOT NULL DEFAULT '',

    FOREIGN KEY(aws_cluster) REFERENCES aws_clusters(cluster),
    FOREIGN KEY(credential) REFERENCES credentials(id)
  );

  CREATE TABLE IF NOT EXISTS prompts (
    id TEXT NOT NULL PRIMARY KEY,
    cluster TEXT NOT NULL,
    type TEXT NOT NULL,
    message TEXT NOT NULL DEFAULT '',
    yes INTEGER NOT NULL DEFAULT 0,
    input TEXT NOT NULL DEFAULT '',
    resolved INTEGER NOT NULL DEFAULT 0,

    FOREIGN KEY(cluster) REFERENCES clusters(id)
  );

  CREATE TABLE IF NOT EXISTS events (
    id TEXT NOT NULL PRIMARY KEY,
    cluster TEXT NOT NULL DEFAULT '',
    prompt TEXT NOT NULL DEFAULT '',
    type TEXT NOT NULL,
    timestamp TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',

    FOREIGN KEY(cluster) REFERENCES clusters(id),
    FOREIGN KEY(prompt) REFERENCES prompts(id)
  );
  `
	_, err := i.db.Exec(sqlStr)
	return err
}
