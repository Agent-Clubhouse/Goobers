-- goobers:binary-version 0.7.0
-- goobers:min-compatible-binary 0.6.0

ALTER TABLE control.config_generation_pin
    ADD COLUMN reader_holder text,
    ADD COLUMN reader_epoch bigint,
    ADD CONSTRAINT config_generation_reader_pin_pair
        CHECK (
            (reader_holder IS NULL) = (reader_epoch IS NULL)
            AND (reader_epoch IS NULL OR reader_epoch > 0)
        );
