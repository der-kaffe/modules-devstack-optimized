-- The original Glance catalog at alembic revision 2024_1_contract01, as
-- mysqldump --no-data shows it in DevStack 2026.2 (MySQL 8). Applied with
-- FOREIGN_KEY_CHECKS=0 by glance-manage db sync, then alembic_version.
CREATE TABLE `alembic_version` (
  `version_num` varchar(32) NOT NULL,
  PRIMARY KEY (`version_num`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3;

CREATE TABLE `cached_images` (
  `id` bigint NOT NULL AUTO_INCREMENT,
  `image_id` varchar(36) NOT NULL,
  `last_accessed` datetime NOT NULL,
  `last_modified` datetime NOT NULL,
  `size` bigint NOT NULL,
  `hits` int NOT NULL,
  `checksum` varchar(32) DEFAULT NULL,
  `node_reference_id` bigint NOT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `ix_cached_images_image_id_node_reference_id` (`image_id`,`node_reference_id`),
  KEY `node_reference_id` (`node_reference_id`),
  CONSTRAINT `cached_images_ibfk_1` FOREIGN KEY (`node_reference_id`) REFERENCES `node_reference` (`node_reference_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3;

CREATE TABLE `image_locations` (
  `id` int NOT NULL AUTO_INCREMENT,
  `image_id` varchar(36) NOT NULL,
  `value` text NOT NULL,
  `created_at` datetime NOT NULL,
  `updated_at` datetime DEFAULT NULL,
  `deleted_at` datetime DEFAULT NULL,
  `deleted` tinyint(1) NOT NULL,
  `meta_data` text,
  `status` varchar(30) NOT NULL DEFAULT 'active',
  PRIMARY KEY (`id`),
  KEY `ix_image_locations_deleted` (`deleted`),
  KEY `ix_image_locations_image_id` (`image_id`),
  CONSTRAINT `image_locations_ibfk_1` FOREIGN KEY (`image_id`) REFERENCES `images` (`id`),
  CONSTRAINT `image_locations_chk_1` CHECK ((`deleted` in (0,1)))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3;

CREATE TABLE `image_members` (
  `id` int NOT NULL AUTO_INCREMENT,
  `image_id` varchar(36) NOT NULL,
  `member` varchar(255) NOT NULL,
  `can_share` tinyint(1) NOT NULL,
  `created_at` datetime NOT NULL,
  `updated_at` datetime DEFAULT NULL,
  `deleted_at` datetime DEFAULT NULL,
  `deleted` tinyint(1) NOT NULL,
  `status` varchar(20) NOT NULL DEFAULT 'pending',
  PRIMARY KEY (`id`),
  UNIQUE KEY `image_members_image_id_member_deleted_at_key` (`image_id`,`member`,`deleted_at`),
  KEY `ix_image_members_deleted` (`deleted`),
  KEY `ix_image_members_image_id` (`image_id`),
  KEY `ix_image_members_image_id_member` (`image_id`,`member`),
  CONSTRAINT `image_members_ibfk_1` FOREIGN KEY (`image_id`) REFERENCES `images` (`id`),
  CONSTRAINT `image_members_chk_1` CHECK ((`can_share` in (0,1))),
  CONSTRAINT `image_members_chk_2` CHECK ((`deleted` in (0,1)))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3;

CREATE TABLE `image_properties` (
  `id` int NOT NULL AUTO_INCREMENT,
  `image_id` varchar(36) NOT NULL,
  `name` varchar(255) NOT NULL,
  `value` text,
  `created_at` datetime NOT NULL,
  `updated_at` datetime DEFAULT NULL,
  `deleted_at` datetime DEFAULT NULL,
  `deleted` tinyint(1) NOT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `ix_image_properties_image_id_name` (`image_id`,`name`),
  KEY `ix_image_properties_deleted` (`deleted`),
  KEY `ix_image_properties_image_id` (`image_id`),
  CONSTRAINT `image_properties_ibfk_1` FOREIGN KEY (`image_id`) REFERENCES `images` (`id`),
  CONSTRAINT `image_properties_chk_1` CHECK ((`deleted` in (0,1)))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3;

CREATE TABLE `image_tags` (
  `id` int NOT NULL AUTO_INCREMENT,
  `image_id` varchar(36) NOT NULL,
  `value` varchar(255) NOT NULL,
  `created_at` datetime NOT NULL,
  `updated_at` datetime DEFAULT NULL,
  `deleted_at` datetime DEFAULT NULL,
  `deleted` tinyint(1) NOT NULL,
  PRIMARY KEY (`id`),
  KEY `ix_image_tags_image_id` (`image_id`),
  KEY `ix_image_tags_image_id_tag_value` (`image_id`,`value`),
  CONSTRAINT `image_tags_ibfk_1` FOREIGN KEY (`image_id`) REFERENCES `images` (`id`),
  CONSTRAINT `image_tags_chk_1` CHECK ((`deleted` in (0,1)))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3;

CREATE TABLE `images` (
  `id` varchar(36) NOT NULL,
  `name` varchar(255) DEFAULT NULL,
  `size` bigint DEFAULT NULL,
  `status` varchar(30) NOT NULL,
  `created_at` datetime NOT NULL,
  `updated_at` datetime DEFAULT NULL,
  `deleted_at` datetime DEFAULT NULL,
  `deleted` tinyint(1) NOT NULL,
  `disk_format` varchar(20) DEFAULT NULL,
  `container_format` varchar(20) DEFAULT NULL,
  `checksum` varchar(32) DEFAULT NULL,
  `owner` varchar(255) DEFAULT NULL,
  `min_disk` int NOT NULL,
  `min_ram` int NOT NULL,
  `protected` tinyint(1) NOT NULL DEFAULT '0',
  `virtual_size` bigint DEFAULT NULL,
  `visibility` enum('private','public','shared','community') NOT NULL DEFAULT 'shared',
  `os_hidden` tinyint(1) NOT NULL DEFAULT '0',
  `os_hash_algo` varchar(64) DEFAULT NULL,
  `os_hash_value` varchar(128) DEFAULT NULL,
  PRIMARY KEY (`id`),
  KEY `checksum_image_idx` (`checksum`),
  KEY `ix_images_deleted` (`deleted`),
  KEY `owner_image_idx` (`owner`),
  KEY `created_at_image_idx` (`created_at`),
  KEY `updated_at_image_idx` (`updated_at`),
  KEY `visibility_image_idx` (`visibility`),
  KEY `os_hidden_image_idx` (`os_hidden`),
  KEY `os_hash_value_image_idx` (`os_hash_value`),
  CONSTRAINT `images_chk_2` CHECK ((`deleted` in (0,1))),
  CONSTRAINT `images_chk_3` CHECK ((`protected` in (0,1)))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3;

CREATE TABLE `metadef_namespace_resource_types` (
  `resource_type_id` int NOT NULL,
  `namespace_id` int NOT NULL,
  `properties_target` varchar(80) DEFAULT NULL,
  `prefix` varchar(80) DEFAULT NULL,
  `created_at` datetime NOT NULL,
  `updated_at` datetime DEFAULT NULL,
  PRIMARY KEY (`resource_type_id`,`namespace_id`),
  KEY `ix_metadef_ns_res_types_namespace_id` (`namespace_id`),
  CONSTRAINT `metadef_namespace_resource_types_ibfk_1` FOREIGN KEY (`namespace_id`) REFERENCES `metadef_namespaces` (`id`),
  CONSTRAINT `metadef_namespace_resource_types_ibfk_2` FOREIGN KEY (`resource_type_id`) REFERENCES `metadef_resource_types` (`id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3;

CREATE TABLE `metadef_namespaces` (
  `id` int NOT NULL AUTO_INCREMENT,
  `namespace` varchar(80) NOT NULL,
  `display_name` varchar(80) DEFAULT NULL,
  `description` text,
  `visibility` varchar(32) DEFAULT NULL,
  `protected` tinyint(1) DEFAULT NULL,
  `owner` varchar(255) NOT NULL,
  `created_at` datetime NOT NULL,
  `updated_at` datetime DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uq_metadef_namespaces_namespace` (`namespace`),
  KEY `ix_metadef_namespaces_owner` (`owner`),
  CONSTRAINT `metadef_namespaces_chk_1` CHECK ((`protected` in (0,1)))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3;

CREATE TABLE `metadef_objects` (
  `id` int NOT NULL AUTO_INCREMENT,
  `namespace_id` int NOT NULL,
  `name` varchar(80) NOT NULL,
  `description` text,
  `required` text,
  `json_schema` text NOT NULL,
  `created_at` datetime NOT NULL,
  `updated_at` datetime DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uq_metadef_objects_namespace_id_name` (`namespace_id`,`name`),
  KEY `ix_metadef_objects_name` (`name`),
  CONSTRAINT `metadef_objects_ibfk_1` FOREIGN KEY (`namespace_id`) REFERENCES `metadef_namespaces` (`id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3;

CREATE TABLE `metadef_properties` (
  `id` int NOT NULL AUTO_INCREMENT,
  `namespace_id` int NOT NULL,
  `name` varchar(80) NOT NULL,
  `json_schema` text NOT NULL,
  `created_at` datetime NOT NULL,
  `updated_at` datetime DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uq_metadef_properties_namespace_id_name` (`namespace_id`,`name`),
  KEY `ix_metadef_properties_name` (`name`),
  CONSTRAINT `metadef_properties_ibfk_1` FOREIGN KEY (`namespace_id`) REFERENCES `metadef_namespaces` (`id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3;

CREATE TABLE `metadef_resource_types` (
  `id` int NOT NULL AUTO_INCREMENT,
  `name` varchar(80) NOT NULL,
  `protected` tinyint(1) NOT NULL,
  `created_at` datetime NOT NULL,
  `updated_at` datetime DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uq_metadef_resource_types_name` (`name`),
  CONSTRAINT `metadef_resource_types_chk_1` CHECK ((`protected` in (0,1)))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3;

CREATE TABLE `metadef_tags` (
  `id` int NOT NULL AUTO_INCREMENT,
  `namespace_id` int NOT NULL,
  `name` varchar(80) NOT NULL,
  `created_at` datetime NOT NULL,
  `updated_at` datetime DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uq_metadef_tags_namespace_id_name` (`namespace_id`,`name`),
  KEY `ix_metadef_tags_name` (`name`),
  CONSTRAINT `metadef_tags_ibfk_1` FOREIGN KEY (`namespace_id`) REFERENCES `metadef_namespaces` (`id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3;

CREATE TABLE `node_reference` (
  `node_reference_id` bigint NOT NULL AUTO_INCREMENT,
  `node_reference_url` varchar(255) NOT NULL,
  PRIMARY KEY (`node_reference_id`),
  UNIQUE KEY `uq_node_reference_node_reference_url` (`node_reference_url`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3;

CREATE TABLE `task_info` (
  `task_id` varchar(36) NOT NULL,
  `input` text,
  `result` text,
  `message` text,
  PRIMARY KEY (`task_id`),
  CONSTRAINT `task_info_ibfk_1` FOREIGN KEY (`task_id`) REFERENCES `tasks` (`id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3;

CREATE TABLE `tasks` (
  `id` varchar(36) NOT NULL,
  `type` varchar(30) NOT NULL,
  `status` varchar(30) NOT NULL,
  `owner` varchar(255) NOT NULL,
  `expires_at` datetime DEFAULT NULL,
  `created_at` datetime NOT NULL,
  `updated_at` datetime DEFAULT NULL,
  `deleted_at` datetime DEFAULT NULL,
  `deleted` tinyint(1) NOT NULL,
  `image_id` varchar(36) DEFAULT NULL,
  `request_id` varchar(64) DEFAULT NULL,
  `user_id` varchar(64) DEFAULT NULL,
  PRIMARY KEY (`id`),
  KEY `ix_tasks_deleted` (`deleted`),
  KEY `ix_tasks_owner` (`owner`),
  KEY `ix_tasks_status` (`status`),
  KEY `ix_tasks_type` (`type`),
  KEY `ix_tasks_updated_at` (`updated_at`),
  KEY `ix_tasks_image_id` (`image_id`),
  CONSTRAINT `tasks_chk_1` CHECK ((`deleted` in (0,1)))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3;

